<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  confirmAiCandidate,
  createAiTask,
  deleteFile,
  fileThumbnailUrl,
  getAiSettings,
  getAiTask,
  listLocations,
  listSpaces,
  newIdempotencyKey,
  uploadFileWithProgress,
} from '../../api'
import { errorText, isNotImplemented } from '../../api/errors'
import type { AiCandidate, AiMaskedSettings, AiTask, TreeNode } from '../../api/types'

// AI 识别入库。链路 = 选库 → 选图/上传 → /ai/tasks(202) → 候选核对 → 逐条确认。
// operation_id 在「同一批上传内容」内保持不变（上传→任务→确认都从它派生），
// 内容变了才换新键；确认的幂等键按「候选 + 本次编辑内容」稳定，重试复用。

const route = useRoute()
const router = useRouter()

const MAX_FALLBACK = 8
interface Pick {
  key: string
  file: File
  preview: string
  id?: string
  status: 'queued' | 'uploading' | 'done' | 'failed'
  pct: number
  error?: string
}
interface ConfirmRow {
  status: 'idle' | 'running' | 'done' | 'failed'
  message?: string
  itemId?: string
  publicCode?: string
  movementId?: string | null
  key?: string
  sig?: string
}
interface Edit {
  name: string
  fields: Record<string, unknown>
}

const loadingEnv = ref(true)
const envError = ref('')
const spaces = ref<TreeNode[]>([])
const locations = ref<TreeNode[]>([])
const settings = ref<AiMaskedSettings | null>(null)
const spaceId = ref('')
const locationId = ref('')

const picks = ref<Pick[]>([])
const uploading = ref(false)
const uploadError = ref('')
const aborters = new Map<string, AbortController>()

const task = ref<AiTask | null>(null)
const candidates = ref<AiCandidate[]>([])
const recognizing = ref(false)
const taskError = ref('')
const taskKey = ref('')
const taskSig = ref('')
const operationId = ref('')

const selections = reactive(new Set<string>())
const photosMode = ref<'share' | 'none'>('share')
const edits = reactive<Record<string, Edit>>({})
const rows = reactive<Record<string, ConfirmRow>>({})
const confirming = ref(false)

const maxImages = computed(() => settings.value?.max_images || MAX_FALLBACK)
const aiReady = computed(() => settings.value?.enabled !== false && !settings.value?.rekey_needed)
const uploadedIds = computed(() => picks.value.filter((p) => p.status === 'done' && p.id).map((p) => p.id!))
const allUploaded = computed(() => picks.value.length > 0 && picks.value.every((p) => p.status === 'done'))
const selectedCandidates = computed(() => candidates.value.filter((c) => selections.has(c.id) && c.state === 'pending'))
const doneRows = computed(() => Object.entries(rows).filter(([, r]) => r.status === 'done'))
const failedRows = computed(() => Object.entries(rows).filter(([, r]) => r.status === 'failed'))
const step = computed(() => {
  if (task.value && candidates.value.length > 0) return doneRows.value.length > 0 || failedRows.value.length > 0 ? 3 : 2
  return picks.value.length > 0 ? 1 : 0
})

function rowOf(id: string): ConfirmRow {
  if (!rows[id]) rows[id] = { status: 'idle' }
  return rows[id]
}

function editOf(c: AiCandidate): Edit {
  if (!edits[c.id]) edits[c.id] = { name: c.name, fields: { ...(c.fields ?? {}) } }
  return edits[c.id]
}

async function loadEnv() {
  loadingEnv.value = true
  envError.value = ''
  try {
    spaces.value = await listSpaces()
    settings.value = await getAiSettings()
    locations.value = await listLocations().catch(() => [])
  } catch (err) {
    envError.value = errorText(err, '初始化失败')
  } finally {
    loadingEnv.value = false
  }
}

function addFiles(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = Array.from(input.files ?? [])
  input.value = ''
  uploadError.value = ''
  for (const f of files) {
    if (!['image/jpeg', 'image/png', 'image/webp'].includes(f.type)) {
      ElMessage.warning(`${f.name}：只支持 JPEG/PNG/WebP`)
      continue
    }
    if (picks.value.length >= maxImages.value) {
      ElMessage.warning(`最多 ${maxImages.value} 张（服务端配置），已忽略其余图片`)
      break
    }
    picks.value.push({ key: newIdempotencyKey(), file: f, preview: URL.createObjectURL(f), status: 'queued', pct: 0 })
  }
}

async function uploadAll() {
  uploading.value = true
  uploadError.value = ''
  try {
    for (const p of picks.value) {
      if (p.status === 'done') continue
      p.status = 'uploading'
      p.pct = 0
      p.error = undefined
      const ac = new AbortController()
      aborters.set(p.key, ac)
      try {
        const fo = await uploadFileWithProgress(p.file, (pct) => (p.pct = pct), ac.signal)
        p.id = fo.id
        p.status = 'done'
        p.pct = 100
      } catch (err) {
        p.status = (err as Error)?.name === 'AbortError' ? 'queued' : 'failed'
        p.pct = 0
        p.error = (err as Error)?.name === 'AbortError' ? '已取消' : errorText(err, '上传失败')
      } finally {
        aborters.delete(p.key)
      }
    }
  } finally {
    uploading.value = false
  }
}

function cancelUploads() {
  aborters.forEach((a) => a.abort())
}

function removePick(p: Pick) {
  if (p.id) void deleteFile(p.id).catch(() => {})
  URL.revokeObjectURL(p.preview)
  picks.value = picks.value.filter((x) => x.key !== p.key)
  // 图片集合变了：旧的幂等键/任务不再对应当前内容，换新批次。
  taskKey.value = ''
  taskSig.value = ''
}

function applyTask(r: { task: AiTask; candidates: AiCandidate[] }) {
  task.value = r.task
  candidates.value = r.candidates
  for (const c of r.candidates) {
    editOf(c)
    const row = rowOf(c.id)
    if (c.state === 'confirmed' && c.confirmed_item_id) {
      row.status = 'done'
      row.itemId = c.confirmed_item_id
    }
  }
  if (r.task.space_id) spaceId.value = r.task.space_id
}

async function startRecognize() {
  if (!spaceId.value) {
    ElMessage.warning('先选择目标库')
    return
  }
  if (picks.value.length === 0) {
    ElMessage.warning('先选择图片')
    return
  }
  await uploadAll()
  if (!allUploaded.value) {
    uploadError.value = '有图片上传失败，请先处理（可重试或移除）'
    return
  }
  const ids = uploadedIds.value
  const sig = JSON.stringify([spaceId.value, ids])
  if (!taskKey.value || taskSig.value !== sig) {
    taskKey.value = newIdempotencyKey()
    taskSig.value = sig
    operationId.value = taskKey.value
  }
  recognizing.value = true
  taskError.value = ''
  try {
    const r = await createAiTask(
      { space_id: spaceId.value, file_ids: ids, location_id: locationId.value || undefined },
      taskKey.value,
    )
    applyTask(r)
    await router.replace({ name: 'ai-import', query: { task: r.task.id } })
    if (r.task.state === 'failed') {
      // 任务已生成（202 终态 failed），同键重放只会拿回同一条失败任务，重试必须换新键。
      taskError.value = r.task.error ? `识别失败：${r.task.error}` : '识别失败，请重试'
      taskKey.value = ''
      taskSig.value = ''
    } else if (r.candidates.length === 0) {
      taskError.value = '没有识别出物品，请换一张更清晰的图片再试'
      taskKey.value = ''
      taskSig.value = ''
    }
  } catch (err) {
    taskError.value = isNotImplemented(err) ? '功能未开放' : errorText(err, '识别失败')
  } finally {
    recognizing.value = false
  }
}

function toggleAll(checked: boolean) {
  for (const c of candidates.value) {
    if (c.state !== 'pending') continue
    if (checked) selections.add(c.id)
    else selections.delete(c.id)
  }
}

async function confirmOne(c: AiCandidate): Promise<boolean> {
  const row = rowOf(c.id)
  const edit = editOf(c)
  const body = {
    space_id: spaceId.value,
    location_id: locationId.value || undefined,
    name: edit.name.trim() || undefined,
    fields: edit.fields,
    quantity: locationId.value ? '1' : undefined,
    // share = 不传 photo_ids + share_photos=true（共享全部来源图）；none = photo_ids: [] 明确不关联。
    photo_ids: photosMode.value === 'none' ? [] : null,
    share_photos: photosMode.value === 'share' ? true : undefined,
  }
  const sig = JSON.stringify(body)
  if (!row.key || row.sig !== sig) {
    row.key = newIdempotencyKey()
    row.sig = sig
  }
  row.status = 'running'
  row.message = undefined
  try {
    const r = await confirmAiCandidate(c.id, body, row.key)
    row.status = 'done'
    row.itemId = r.item_id
    row.publicCode = r.public_code
    row.movementId = r.movement_id
    c.state = 'confirmed'
    c.confirmed_item_id = r.item_id
    return true
  } catch (err) {
    row.status = 'failed'
    row.message = isNotImplemented(err) ? '功能未开放' : errorText(err, '确认入库失败')
    return false
  }
}

async function confirmSelected() {
  const list = selectedCandidates.value
  if (list.length === 0) {
    ElMessage.warning('先勾选要入库的候选')
    return
  }
  if (!spaceId.value) {
    ElMessage.warning('先选择目标库')
    return
  }
  confirming.value = true
  try {
    for (const c of list) await confirmOne(c)
  } finally {
    confirming.value = false
  }
}

async function retryFailed() {
  const list = failedRows.value
    .map(([id]) => candidates.value.find((c) => c.id === id))
    .filter((c): c is AiCandidate => !!c)
  if (list.length === 0) return
  confirming.value = true
  try {
    for (const c of list) await confirmOne(c)
  } finally {
    confirming.value = false
  }
}

async function loadTaskById(id: string) {
  loadingEnv.value = true
  envError.value = ''
  try {
    const r = await getAiTask(id)
    applyTask(r)
  } catch (err) {
    envError.value = errorText(err, '任务加载失败')
  } finally {
    loadingEnv.value = false
  }
}

async function resetBatch() {
  for (const p of picks.value) {
    if (p.id) void deleteFile(p.id).catch(() => {})
    URL.revokeObjectURL(p.preview)
  }
  picks.value = []
  task.value = null
  candidates.value = []
  Object.keys(rows).forEach((k) => delete rows[k])
  Object.keys(edits).forEach((k) => delete edits[k])
  selections.clear()
  taskKey.value = ''
  taskSig.value = ''
  operationId.value = ''
  taskError.value = ''
  uploadError.value = ''
  photosMode.value = 'share'
  await router.replace({ name: 'ai-import' })
}

onMounted(async () => {
  await loadEnv()
  const q = typeof route.query.task === 'string' ? route.query.task : ''
  if (q) await loadTaskById(q)
})

onBeforeUnmount(() => {
  aborters.forEach((a) => a.abort())
  picks.value.forEach((p) => URL.revokeObjectURL(p.preview))
})
</script>

<template>
  <div v-loading="loadingEnv" class="ai-page">
    <el-card class="block">
      <template #header>
        <div class="head">
          <span>AI 识别入库</span>
          <el-steps :active="step" simple class="steps">
            <el-step title="选库选图" />
            <el-step title="识别" />
            <el-step title="核对确认" />
            <el-step title="完成" />
          </el-steps>
        </div>
      </template>

      <el-alert v-if="envError" :title="envError" type="error" :closable="false" class="block">
        <el-button size="small" @click="loadEnv">重试</el-button>
      </el-alert>

      <el-alert
        v-if="!aiReady && settings"
        type="warning"
        :closable="false"
        class="block"
        show-icon
        title="AI 识别未配置或未启用"
      >
        <router-link to="/settings/ai">去设置页配置提供者、模型与密钥</router-link>
      </el-alert>

      <!-- ① 目标库 -->
      <el-form label-width="96px" class="block">
        <el-form-item label="目标库" required>
          <el-select v-model="spaceId" placeholder="选择识别结果要入库的库" style="width: 320px" :disabled="!!task">
            <el-option v-for="s in spaces" :key="s.id" :label="s.name" :value="s.id" />
          </el-select>
          <el-button text type="primary" class="ml" @click="loadEnv">刷新仓库</el-button>
        </el-form-item>
        <el-form-item label="入库位置">
          <el-select v-model="locationId" placeholder="不选=待入库（无初始库存）" clearable style="width: 320px">
            <el-option v-for="l in locations" :key="l.id" :label="l.name" :value="l.id" />
          </el-select>
        </el-form-item>
      </el-form>

      <el-empty v-if="!loadingEnv && spaces.length === 0" description="暂无仓库，请先在基础数据里创建">
        <el-button @click="router.push('/bases')">去基础数据</el-button>
      </el-empty>

      <!-- ② 选图 / 上传 -->
      <template v-if="spaces.length > 0">
        <div class="upload-row">
          <label class="file-btn">
            选择图片（JPEG/PNG/WebP，最多 {{ maxImages }} 张）
            <input
              type="file"
              accept="image/jpeg,image/png,image/webp"
              multiple
              hidden
              :disabled="!!task"
              @change="addFiles"
            />
          </label>
          <el-text type="info" size="small">已选 {{ picks.length }}/{{ maxImages }}</el-text>
          <el-button v-if="uploading" size="small" @click="cancelUploads">取消上传</el-button>
        </div>

        <div v-if="picks.length" class="picks">
          <div v-for="p in picks" :key="p.key" class="pick">
            <img :src="p.preview" class="pick-img" alt="" />
            <div class="pick-meta">
              <div class="pick-name">{{ p.file.name }}</div>
              <el-progress
                v-if="p.status === 'uploading'"
                :percentage="p.pct"
                :stroke-width="6"
                :show-text="false"
              />
              <el-text v-if="p.status === 'failed'" type="danger" size="small">{{ p.error }}</el-text>
              <el-text v-else-if="p.status === 'done'" type="success" size="small">已上传</el-text>
              <el-text v-else type="info" size="small">{{ p.status === 'queued' ? '待上传' : '上传中' }}</el-text>
            </div>
            <el-button text type="danger" size="small" :disabled="uploading" @click="removePick(p)">移除</el-button>
          </div>
        </div>

        <el-alert v-if="uploadError" :title="uploadError" type="error" :closable="false" class="block" />

        <div class="block">
          <el-button type="primary" :loading="recognizing || uploading" :disabled="!spaceId || picks.length === 0" @click="startRecognize">
            {{ recognizing ? '识别中…' : '上传并开始识别' }}
          </el-button>
          <el-button v-if="task" @click="resetBatch">重新开始</el-button>
        </div>

        <el-alert v-if="taskError" :title="taskError" type="error" :closable="false" class="block">
          <el-button size="small" :loading="recognizing" @click="startRecognize">重试</el-button>
        </el-alert>
      </template>
    </el-card>

    <!-- ③ 候选核对 -->
    <el-card v-if="candidates.length" class="block">
      <template #header>
        <div class="head">
          <span>识别到 {{ candidates.length }} 条候选</span>
          <span class="head-actions">
            <el-checkbox
              :model-value="selectedCandidates.length === candidates.filter((c) => c.state === 'pending').length && selectedCandidates.length > 0"
              @change="(v: boolean) => toggleAll(v)"
            >
              全选待处理
            </el-checkbox>
            <el-radio-group v-model="photosMode" size="small">
              <el-radio value="share">关联来源图片</el-radio>
              <el-radio value="none">不关联图片</el-radio>
            </el-radio-group>
          </span>
        </div>
      </template>

      <div v-for="c in candidates" :key="c.id" class="cand">
        <div class="cand-head">
          <el-checkbox
            :model-value="selections.has(c.id)"
            :disabled="c.state !== 'pending' || rows[c.id]?.status === 'done'"
            @change="(v: boolean) => (v ? selections.add(c.id) : selections.delete(c.id))"
          />
          <el-input v-model="editOf(c).name" size="small" class="cand-name" :disabled="c.state !== 'pending'" />
          <el-tag v-if="c.confidence" size="small" type="info">{{ Math.round((c.confidence ?? 0) * 100) }}%</el-tag>
          <el-tag v-if="rows[c.id]?.status === 'done'" type="success" size="small">
            已入库 {{ rows[c.id].publicCode }}（{{ rows[c.id].itemId }}）
          </el-tag>
          <el-tag v-else-if="rows[c.id]?.status === 'failed'" type="danger" size="small">失败</el-tag>
        </div>

        <el-alert
          v-if="rows[c.id]?.message"
          :title="rows[c.id].message"
          type="error"
          :closable="false"
          class="cand-alert"
        />

        <div class="cand-body">
          <div class="cand-photos">
            <el-image
              v-for="fid in c.source_image_ids"
              :key="fid"
              :src="fileThumbnailUrl(fid)"
              :preview-src-list="c.source_image_ids.map(fileThumbnailUrl)"
              fit="cover"
              class="cand-photo"
              preview-teleported
            />
          </div>
          <div class="cand-info">
            <el-text v-if="c.summary" size="small">{{ c.summary }}</el-text>
            <el-text v-if="c.evidence" type="info" size="small">依据：{{ c.evidence }}</el-text>
            <el-text v-if="c.notes" type="info" size="small">备注：{{ c.notes }}</el-text>
            <el-text v-for="w in c.warnings ?? []" :key="w" type="warning" size="small">⚠ {{ w }}</el-text>
            <div class="fields">
              <div v-for="(_v, k) in editOf(c).fields" :key="k" class="field">
                <span class="field-key">{{ k }}</span>
                <el-input v-model="editOf(c).fields[k]" size="small" :disabled="c.state !== 'pending'" />
              </div>
            </div>
          </div>
        </div>
      </div>

      <div class="block">
        <el-button type="primary" :loading="confirming" :disabled="selectedCandidates.length === 0" @click="confirmSelected">
          确认选中 {{ selectedCandidates.length }} 条
        </el-button>
        <el-button v-if="failedRows.length" type="warning" :loading="confirming" @click="retryFailed">
          只重试失败项（{{ failedRows.length }}）
        </el-button>
        <el-text type="info" size="small">
          位置：{{ locationId ? '已选，确认后直接入库' : '未选，确认后为待入库' }}；照片：{{
            photosMode === 'share' ? '关联来源图片' : '不关联图片'
          }}
        </el-text>
      </div>
    </el-card>

    <!-- ④ 结果 -->
    <el-card v-if="doneRows.length" class="block">
      <template #header>已入库 {{ doneRows.length }} 件</template>
      <el-table :data="doneRows.map(([id, r]) => ({ id, ...r }))" size="small">
        <el-table-column prop="publicCode" label="编号" width="160" />
        <el-table-column prop="itemId" label="物品 ID" />
        <el-table-column prop="movementId" label="流水 ID" />
        <el-table-column label="操作" width="120">
          <template #default="{ row }">
            <el-button text type="primary" size="small" @click="router.push(`/items/${row.itemId}`)">查看</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </div>
</template>

<style scoped>
.ai-page {
  max-width: 1080px;
}
.block {
  margin-bottom: 16px;
}
.head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.head-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}
.steps {
  flex: 1;
  min-width: 380px;
  max-width: 560px;
}
.upload-row {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}
.file-btn {
  display: inline-block;
  padding: 8px 16px;
  border: 1px dashed var(--el-color-primary);
  border-radius: 6px;
  color: var(--el-color-primary);
  cursor: pointer;
  font-size: 14px;
}
.picks {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 12px;
  margin-bottom: 12px;
}
.pick {
  display: flex;
  gap: 8px;
  align-items: center;
  border: 1px solid #ebeef5;
  border-radius: 6px;
  padding: 8px;
}
.pick-img {
  width: 56px;
  height: 56px;
  object-fit: cover;
  border-radius: 4px;
}
.pick-meta {
  flex: 1;
  min-width: 0;
}
.pick-name {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cand {
  border: 1px solid #ebeef5;
  border-radius: 6px;
  padding: 12px;
  margin-bottom: 12px;
}
.cand-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.cand-name {
  width: 260px;
}
.cand-alert {
  margin-bottom: 8px;
}
.cand-body {
  display: flex;
  gap: 16px;
}
.cand-photos {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.cand-photo {
  width: 96px;
  height: 96px;
  border-radius: 4px;
}
.cand-info {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.fields {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 8px;
  margin-top: 4px;
}
.field {
  display: flex;
  align-items: center;
  gap: 6px;
}
.field-key {
  font-size: 12px;
  color: #909399;
  width: 90px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ml {
  margin-left: 8px;
}
</style>
