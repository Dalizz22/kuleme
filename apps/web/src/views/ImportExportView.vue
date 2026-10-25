<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { UploadRequestOptions } from 'element-plus'
import {
  listSpaces,
  listLocations,
  listTemplates,
  createImport,
  getImport,
  confirmImport,
  requestExport,
  listExports,
  exportDownloadUrl,
  deleteExport,
  newIdempotencyKey,
} from '../api'
import { errorText } from '../api/errors'
import type { TreeNode, TemplateManifest } from '../api/types'
import type { ImportBatch as IB, ImportRow as IR, ImportMode, ExportFile as EF } from '../api'

const spaces = ref<TreeNode[]>([])
const templates = ref<TemplateManifest[]>([])
const locations = ref<TreeNode[]>([])

// 导入（二选一：仅导入目录 / 直接入库）
const importSpace = ref('')
const importTemplate = ref('')
const importMode = ref<ImportMode>('catalog')
const importLocation = ref('')
const uploading = ref(false)
const batch = ref<IB | null>(null)
const rows = ref<IR[]>([])
const confirming = ref(false)

// 上传每次新键（multipart 摘要按原始字节，同键必 409）；确认键稳定复用直到本次确认落终态。
const confirmKey = ref('')
const pendingFile = ref<File | null>(null)
const importError = ref('')

// 导出
const exports = ref<EF[]>([])
const exporting = ref(false)
const exportTemplate = ref('')
const metaError = ref('')

function msg(err: unknown, fallback: string): string {
  return errorText(err, fallback)
}

async function loadMeta() {
  metaError.value = ''
  try {
    const [s, t, l, ex] = await Promise.all([listSpaces(), listTemplates(), listLocations(), listExports()])
    spaces.value = s
    templates.value = t.items
    locations.value = l
    exports.value = ex.items
  } catch (err) {
    metaError.value = msg(err, '数据加载失败')
  }
}
onMounted(loadMeta)

// el-upload 自定义请求：只记文件，提交由「开始导入」按钮触发（选 stock 未选位置要先拦住）。
function pickFile(opt: UploadRequestOptions) {
  pendingFile.value = opt.file
  importError.value = ''
}

const canSubmit = computed(() => {
  if (!importSpace.value || !importTemplate.value || !pendingFile.value) return false
  return importMode.value === 'catalog' || Boolean(importLocation.value)
})

async function submitImport() {
  const file = pendingFile.value
  if (!file || !canSubmit.value) return
  uploading.value = true
  importError.value = ''
  try {
    const d = await createImport(
      file,
      importSpace.value,
      importTemplate.value,
      importMode.value,
      importMode.value === 'stock' ? importLocation.value : undefined,
    )
    batch.value = d.batch
    rows.value = d.preview
    confirmKey.value = newIdempotencyKey()
    ElMessage.success(`解析完成，共 ${d.batch.total} 行，请核对后确认`)
  } catch (err) {
    // 坏位置等整批拒绝：保留文件与键，给重试入口；改设置会换新键。
    importError.value = msg(err, '导入解析失败')
  } finally {
    uploading.value = false
  }
}

async function confirm() {
  if (!batch.value) return
  const id = batch.value.id
  if (!confirmKey.value) confirmKey.value = newIdempotencyKey()
  confirming.value = true
  try {
    const d = await confirmImport(id, confirmKey.value)
    batch.value = d.batch
    rows.value = d.rows
    // 本次确认已落终态：下次确认是新动作，换新键（同键只会重放旧响应）。
    confirmKey.value = newIdempotencyKey()
    ElMessage.success(`确认完成：成功 ${d.batch.ok} 行，失败 ${d.batch.failed} 行`)
  } catch (err) {
    const status = (err as { status?: number })?.status
    if (status === 409) {
      // 已全部成功 / 正在确认中：都不是要重试的错误，别清掉结果。
      ElMessage.warning(msg(err, '该批次已全部入库完成'))
      if (batch.value) {
        try {
          const d = await getImport(batch.value.id)
          batch.value = d.batch
          rows.value = d.rows
        } catch {
          /* 重读失败保留旧结果即可 */
        }
      }
    } else {
      ElMessage.error(msg(err, '确认导入失败'))
    }
  } finally {
    confirming.value = false
  }
}

// 改仓库/模板/模式/位置只丢弃旧批次，保留已选文件（否则用户先选文件再选位置就被清空）。
function resetParams() {
  batch.value = null
  rows.value = []
  importError.value = ''
  confirmKey.value = ''
}

function resetImport() {
  resetParams()
  pendingFile.value = null
}

const BATCH_STATUS_TEXT: Record<string, string> = {
  draft: '待确认',
  confirming: '确认中',
  done: '已完成',
}

function batchStatusText(s: string): string {
  return BATCH_STATUS_TEXT[s] ?? '未知状态'
}

function rowStatusText(r: IR): string {
  if (r.status === 'catalog') return '已入目录'
  if (r.status === 'stock') return '已入库'
  return '失败'
}

const canConfirm = computed(() => {
  const b = batch.value
  if (!b) return false
  if (b.status === 'draft') return true
  // done 且仍有失败行：可再确认，服务端只重跑失败/未完成行。
  return b.status === 'done' && b.failed > 0
})

async function doExport() {
  exporting.value = true
  try {
    await requestExport(exportTemplate.value || undefined)
    ElMessage.success('导出已生成')
    const ex = await listExports()
    exports.value = ex.items
  } catch (err) {
    ElMessage.error(msg(err, '导出失败'))
  } finally {
    exporting.value = false
  }
}

async function removeExport(row: EF) {
  try {
    await ElMessageBox.confirm(`删除导出「${row.file_name}」？删除后任何人（包括你）都无法再下载。`, '确认撤权', { type: 'warning' })
  } catch {
    return
  }
  try {
    await deleteExport(row.id)
    exports.value = exports.value.filter((e) => e.id !== row.id)
    ElMessage.success('已删除')
  } catch (err) {
    ElMessage.error(msg(err, '删除失败'))
  }
}

function fmtSize(n: number): string {
  return n > 1024 * 1024 ? (n / 1024 / 1024).toFixed(1) + ' MB' : Math.ceil(n / 1024) + ' KB'
}
</script>

<template>
  <el-card>
    <template #header>导入导出</template>
    <el-alert v-if="metaError" :title="metaError" type="error" :closable="false" class="block">
      <el-button size="small" @click="loadMeta">重试</el-button>
    </el-alert>
    <el-tabs>
      <el-tab-pane label="导入物品">
        <el-form label-width="90px">
          <el-form-item label="目标仓库">
            <el-select v-model="importSpace" placeholder="选择仓库" style="width: 220px" @change="resetParams">
              <el-option v-for="s in spaces" :key="s.id" :label="s.name" :value="s.id" />
            </el-select>
            <el-select v-model="importTemplate" placeholder="选择模板" style="width: 200px; margin-left: 12px" @change="resetParams">
              <el-option v-for="t in templates" :key="t.template_key" :label="t.name" :value="t.template_key" />
            </el-select>
          </el-form-item>
          <el-form-item label="导入方式">
            <el-radio-group v-model="importMode" @change="resetParams">
              <el-radio value="catalog">仅导入目录（不建库存）</el-radio>
              <el-radio value="stock">直接入库（每行建初始库存）</el-radio>
            </el-radio-group>
          </el-form-item>
          <el-form-item v-if="importMode === 'stock'" label="入库位置" required>
            <el-select v-model="importLocation" placeholder="选择位置" style="width: 340px" @change="resetParams">
              <el-option v-for="l in locations" :key="l.id" :label="l.name" :value="l.id" />
            </el-select>
            <div v-if="!importLocation" class="hint">选「直接入库」必须选择位置，未选时不能提交。</div>
          </el-form-item>
        </el-form>

        <el-upload drag :http-request="pickFile" :show-file-list="false" accept=".csv,.xlsx" :disabled="uploading">
          <div style="padding: 24px">
            拖拽 CSV / XLSX 到这里，或点击选择
            <div style="color: var(--el-text-color-secondary); font-size: 12px; margin-top: 4px">
              首行为表头；只新增不更新；单文件 ≤10MB、≤1000 行（超出会整批拒绝，改小后重传）
            </div>
            <div v-if="pendingFile" style="margin-top: 6px">已选：{{ pendingFile.name }}</div>
          </div>
        </el-upload>

        <div style="margin-top: 12px">
          <el-button type="primary" :loading="uploading" :disabled="!canSubmit" @click="submitImport">
            {{ uploading ? '解析中…' : '开始导入' }}
          </el-button>
          <span v-if="importMode === 'stock' && !importLocation" class="hint" style="margin-left: 8px">
            请先选择入库位置
          </span>
        </div>

        <el-alert v-if="importError" :title="importError" type="error" :closable="false" style="margin-top: 12px">
          <el-button size="small" @click="submitImport">重试（同一批次）</el-button>
        </el-alert>

        <template v-if="batch">
          <el-descriptions :title="`批次 · ${batch.mode === 'stock' ? '直接入库' : '仅导入目录'}`" :column="4" style="margin-top: 16px" border>
            <el-descriptions-item label="状态">{{ batchStatusText(batch.status) }}</el-descriptions-item>
            <el-descriptions-item label="总行数">{{ batch.total }}</el-descriptions-item>
            <el-descriptions-item label="成功">{{ batch.ok }}</el-descriptions-item>
            <el-descriptions-item label="失败">{{ batch.failed }}</el-descriptions-item>
          </el-descriptions>
          <el-alert
            v-if="batch.status === 'done' && batch.failed > 0"
            type="warning"
            :closable="false"
            style="margin: 8px 0"
            title="有失败行：瞬时失败的可以「重试失败行」；内容问题导致的失败无法在本页修正，请修改文件后重新上传（会生成新批次）"
          />
          <el-alert
            v-if="batch.status === 'draft'"
            type="info"
            :closable="false"
            style="margin: 8px 0"
            title="确认后逐行创建物品（失败行可单独重试；已成功的行不会重复创建）"
          />
          <el-table :data="rows" size="small" style="margin-top: 8px">
            <el-table-column prop="row_no" label="行" width="60" />
            <el-table-column label="结果" width="110">
              <template #default="{ row }">
                <el-tag :type="row.status === 'failed' ? 'danger' : 'success'" size="small">{{ rowStatusText(row) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="编号" width="140">
              <template #default="{ row }">{{ row.public_code || '—' }}</template>
            </el-table-column>
            <el-table-column label="失败原因" show-overflow-tooltip>
              <template #default="{ row }">
                <span v-if="row.status === 'failed'">{{ row.error_code ? row.error_code + '：' : '' }}{{ row.message || row.error_code }}</span>
                <span v-else>—</span>
              </template>
            </el-table-column>
          </el-table>
          <div style="margin-top: 12px">
            <el-button v-if="canConfirm" type="primary" :loading="confirming" @click="confirm">
              {{ batch.status === 'draft' ? '确认导入' : '重试失败行' }}
            </el-button>
            <el-button v-if="batch.status === 'done' && batch.failed === 0" type="success" disabled>该批次已全部完成</el-button>
            <el-button @click="resetImport">清空，重新导入</el-button>
          </div>
        </template>
      </el-tab-pane>

      <el-tab-pane label="导出物品">
        <el-form inline>
          <el-form-item label="模板过滤">
            <el-select v-model="exportTemplate" clearable placeholder="全部模板" style="width: 200px">
              <el-option v-for="t in templates" :key="t.template_key" :label="t.name" :value="t.template_key" />
            </el-select>
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="exporting" @click="doExport">生成导出</el-button>
          </el-form-item>
        </el-form>
        <el-table :data="exports">
          <el-table-column prop="file_name" label="文件" />
          <el-table-column prop="row_count" label="行数" width="90" />
          <el-table-column label="大小" width="100">
            <template #default="{ row }">{{ fmtSize(row.byte_size) }}</template>
          </el-table-column>
          <el-table-column label="生成时间" width="180">
            <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="操作" width="160">
            <template #default="{ row }">
              <el-button size="small" type="primary" tag="a" :href="exportDownloadUrl(row.id)">下载</el-button>
              <el-button size="small" type="danger" @click="removeExport(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-empty v-if="!exports.length" description="还没有导出记录" />
      </el-tab-pane>
    </el-tabs>
  </el-card>
</template>

<style scoped>
.hint {
  color: var(--el-color-warning);
  font-size: 12px;
  margin-top: 4px;
}
</style>
