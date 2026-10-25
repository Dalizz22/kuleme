<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  closeMaintenance,
  deleteItem,
  getItem,
  getTemplate,
  inventoryOp,
  listLocations,
  listSpaces,
  newIdempotencyKey,
  openMaintenance,
  saleItem,
  transferItem,
  transferPreview,
} from '../../api'
import { ApiError } from '../../api/request'
import { errorText, isNotImplemented, isUncertainWriteFailure, recoverFailedWrite } from '../../api/errors'
import type { ItemView, TemplateDetail, TreeNode } from '../../api/types'
import ItemPhotoPanel from '../../components/ItemPhotoPanel.vue'

const route = useRoute()
const router = useRouter()
const id = route.params.id as string

const item = ref<ItemView | null>(null)
const tpl = ref<TemplateDetail | null>(null)
const locations = ref<TreeNode[]>([])
const loading = ref(true)
const error = ref('')
const showTransfer = ref(false)
// 库内移位 = inventory move；跨库 = transfer-preview + transfer（服务端拒绝同库转库）。
const transferMode = ref<'move' | 'transfer'>('move')
const spaces = ref<TreeNode[]>([])
const targetSpace = ref('')
const transferTarget = ref('')

// 幂等键：同一操作意图的失败重试复用同一把键，成功后销毁。
const opKeys = new Map<string, string>()
function keyOf(name: string): string {
  let k = opKeys.get(name)
  if (!k) {
    k = newIdempotencyKey()
    opKeys.set(name, k)
  }
  return k
}

// 维修中：服务端 status 派生可靠；maintenance_open 列当前未被 GORM 扫进 JSON（恒 false），
// 两者取或：字段缺失时退回旧推导，服务端补全后自动以字段为准。
const maintenanceOpen = computed(() => item.value?.status === 'maintenance' || !!item.value?.maintenance_open)

// 动作可用性只认服务端 allowed_actions（库权限/功能开关），字段缺失/null/键缺失都当可用；
// false 才禁用，并原文展示 action_blocked_reasons。业务态（已售/维修中）不在这里拦，交给服务端 409 兜底。
function allowed(key: string): boolean {
  return item.value?.allowed_actions?.[key] !== false
}
function blockedReason(key: string): string {
  return item.value?.action_blocked_reasons?.[key] ?? ''
}

const STATUS_TEXT: Record<string, string> = {
  in_stock: '在库',
  pending_stock: '待入库',
  reserved: '预留',
  maintenance: '维修中',
  sold: '已售',
  scrapped: '已报废',
  lost: '已遗失',
}

// 位置下拉：按树深度缩进展示。
const locationOptions = computed(() => {
  const byId = new Map(locations.value.map((l) => [l.id, l]))
  const depth = (n: TreeNode): number => (n.parent_id && byId.has(n.parent_id) ? depth(byId.get(n.parent_id)!) + 1 : 0)
  return locations.value.map((l) => ({ id: l.id, label: '　'.repeat(depth(l)) + l.name }))
})

const fieldRows = computed(() => {
  if (!item.value || !tpl.value) return []
  return tpl.value.manifest.fields.map((f) => ({
    label: f.label,
    value: formatField(item.value!.fields[f.field_id]),
  }))
})

function formatField(v: unknown): string {
  if (v === null || v === undefined || v === '') return '—'
  if (typeof v === 'boolean') return v ? '是' : '否'
  return String(v)
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const it = await getItem(id)
    item.value = it
    const [t, locs, sps] = await Promise.all([
      getTemplate(it.template_key).catch(() => null),
      listLocations().catch(() => []),
      listSpaces().catch(() => []),
    ])
    tpl.value = t
    locations.value = locs
    spaces.value = sps
  } catch (err) {
    error.value = errorText(err, '物品加载失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)

// 错误提示按契约区分：只有 NOT_IMPLEMENTED 才是「功能未开放」；其余显示服务端真实原因。
function reportError(err: unknown, label: string) {
  if (isNotImplemented(err)) {
    ElMessage.info('功能未开放')
    return
  }
  if (err instanceof ApiError && err.code === 'VERSION_CONFLICT') {
    ElMessage.warning('物品已被他人修改，已刷新最新状态，请重试')
    void load()
    return
  }
  ElMessage.error(errorText(err, `${label}失败`))
}

// 写操作失败且可能已提交（断网/5xx/409）：按同一幂等键查终态，绝不换键重复提交。
async function writeFailed(err: unknown, label: string, key: string) {
  if (err instanceof ApiError && err.code === 'VERSION_CONFLICT') {
    reportError(err, label)
    return
  }
  if (!isUncertainWriteFailure(err)) {
    reportError(err, label)
    return
  }
  const r = await recoverFailedWrite(key, err, `${label}失败`)
  if (r.recovered) {
    ElMessage.success(`${label}已生效，已刷新最新状态`)
    await load()
    return
  }
  ElMessage.warning(r.message)
}

async function onScrap() {
  if (!item.value) return
  if (!item.value.location_id) {
    ElMessage.warning('该物品不在任何位置，无法报废')
    return
  }
  try {
    await ElMessageBox.confirm('确认报废该物品？', '报废', { type: 'warning' })
  } catch {
    return
  }
  const key = keyOf('scrap')
  try {
    await inventoryOp({ item_id: id, kind: 'scrap', location_id: item.value.location_id, quantity: '1' }, key)
    opKeys.delete('scrap')
    ElMessage.success('报废成功')
    await load()
  } catch (err) {
    await writeFailed(err, '报废', key)
  }
}

// 维修不是库存操作：送修/结束走 /items/:id/maintenance 生命周期接口。
async function onRepair() {
  if (!item.value) return
  if (maintenanceOpen.value) {
    try {
      await ElMessageBox.confirm('确认结束维修？物品将恢复在库状态。', '结束维修', { type: 'warning' })
    } catch {
      return
    }
    const key = keyOf('repair-close')
    try {
      await closeMaintenance(id, key)
      opKeys.delete('repair-close')
      ElMessage.success('维修已结束')
      await load()
    } catch (err) {
      await writeFailed(err, '结束维修', key)
    }
    return
  }
  let reason: string
  try {
    const res = await ElMessageBox.prompt('维修原因（必填，200 字以内）', '送修', {
      confirmButtonText: '确认送修',
      cancelButtonText: '取消',
      inputValidator: (v) => (v && v.trim().length > 0 && v.trim().length <= 200) || '请填写 200 字以内的维修原因',
    })
    reason = res.value.trim()
  } catch {
    return
  }
  const key = keyOf('repair-open')
  try {
    await openMaintenance(id, { reason }, key)
    opKeys.delete('repair-open')
    ElMessage.success('已送修')
    await load()
  } catch (err) {
    await writeFailed(err, '送修', key)
  }
}

function onTransfer() {
  if (!item.value) return
  transferMode.value = 'move'
  targetSpace.value = ''
  transferTarget.value = ''
  showTransfer.value = true
}

// 库内移位：真实接口是库存 move（连库存一起搬），不是跨库 transfer。
async function doMove(to: string) {
  if (!item.value) return
  const key = keyOf(`move:${to}`)
  try {
    await inventoryOp(
      { item_id: id, kind: 'move', location_id: item.value.location_id, to_location_id: to, quantity: '1' },
      key,
    )
    opKeys.delete(`move:${to}`)
    ElMessage.success('移位成功')
    await load()
  } catch (err) {
    await writeFailed(err, '移位', key)
  }
}

// 跨库转库：先干跑看警告，再带 expected_version 提交；同库会被服务端拒绝并提示用移位。
async function doTransfer(space: string, to: string) {
  if (!item.value) return
  const body = { to_space_id: space, to_location_id: to, expected_version: item.value.version }
  try {
    const preview = await transferPreview(id, body)
    if (preview.warnings.length > 0) {
      try {
        await ElMessageBox.confirm(preview.warnings.join('；'), '转库确认', { type: 'warning' })
      } catch {
        return
      }
    }
  } catch (err) {
    reportError(err, '转库预览')
    return
  }
  const key = keyOf(`transfer:${space}:${to}:${body.expected_version}`)
  try {
    await transferItem(id, body, key)
    opKeys.delete(`transfer:${space}:${to}:${body.expected_version}`)
    ElMessage.success('转库成功')
    await load()
  } catch (err) {
    await writeFailed(err, '转库', key)
  }
}

async function confirmTransfer() {
  const to = transferTarget.value
  if (!to || !item.value) {
    ElMessage.warning('请选择目标位置')
    return
  }
  if (transferMode.value === 'transfer') {
    if (!targetSpace.value) {
      ElMessage.warning('请选择目标库')
      return
    }
    showTransfer.value = false
    await doTransfer(targetSpace.value, to)
    return
  }
  showTransfer.value = false
  await doMove(to)
}

async function onSell() {
  if (!item.value) return
  if (!item.value.location_id) {
    ElMessage.warning('该物品不在任何位置，无法出售')
    return
  }
  let price: string
  try {
    const res = await ElMessageBox.prompt('成交价格（可空，最多两位小数）', '出售', {
      type: 'warning',
      confirmButtonText: '确认出售',
      cancelButtonText: '取消',
      inputValidator: (v) => !v || /^\d+(\.\d{1,2})?$/.test(v.trim()) || '价格格式不正确',
    })
    price = res.value?.trim() ?? ''
  } catch {
    return
  }
  const key = keyOf('sale')
  try {
    await saleItem(id, price ? { price } : {}, key)
    opKeys.delete('sale')
    ElMessage.success('出售成功')
    await load()
  } catch (err) {
    await writeFailed(err, '出售', key)
  }
}

async function onDelete() {
  try {
    await ElMessageBox.confirm('确认删除该物品？删除后进入回收站。', '删除', { type: 'warning' })
  } catch {
    return
  }
  try {
    await deleteItem(id)
    ElMessage.success('已删除')
    await router.push({ name: 'items' })
  } catch (err) {
    reportError(err, '删除')
  }
}
</script>

<template>
  <div v-loading="loading">
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
      <el-button size="small" @click="load">重试</el-button>
    </el-alert>

    <template v-if="item">
      <el-card class="block">
        <template #header>
          <div class="head">
            <span>{{ item.name }} <el-tag size="small">{{ item.public_code }}</el-tag></span>
            <div class="actions">
              <el-button
                type="primary"
                :disabled="!allowed('sale')"
                :title="blockedReason('sale')"
                @click="onSell"
              >
                出售
              </el-button>
              <el-button :disabled="!allowed('scrap')" :title="blockedReason('scrap')" @click="onScrap">报废</el-button>
              <el-button
                :disabled="!allowed('maintenance')"
                :title="blockedReason('maintenance')"
                @click="onRepair"
              >
                {{ maintenanceOpen ? '结束维修' : '维修' }}
              </el-button>
              <el-button :disabled="!allowed('transfer')" :title="blockedReason('transfer')" @click="onTransfer">
                转库
              </el-button>
              <el-button type="danger" plain @click="onDelete">删除</el-button>
            </div>
          </div>
        </template>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="模板">{{ tpl?.manifest.name ?? item.template_key }}</el-descriptions-item>
          <el-descriptions-item label="位置">{{ item.location_path || '—' }}</el-descriptions-item>
          <el-descriptions-item label="数量">{{ item.quantity }}</el-descriptions-item>
          <el-descriptions-item label="状态">{{ STATUS_TEXT[item.status] ?? '未知状态' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ new Date(item.created_at).toLocaleString('zh-CN') }}</el-descriptions-item>
          <el-descriptions-item label="更新时间">{{ new Date(item.updated_at).toLocaleString('zh-CN') }}</el-descriptions-item>
          <el-descriptions-item v-for="f in fieldRows" :key="f.label" :label="f.label">{{ f.value }}</el-descriptions-item>
        </el-descriptions>
      </el-card>

      <el-card class="block">
        <template #header>照片</template>
        <ItemPhotoPanel
          v-if="item"
          :item-id="item.id"
          :primary-file-id="item.photo_file_id"
          @updated="load"
        />
      </el-card>

      <el-dialog v-model="showTransfer" title="转移" width="440px">
        <el-radio-group v-model="transferMode" class="transfer-mode">
          <el-radio value="move">库内移位</el-radio>
          <el-radio value="transfer">转库到其他库</el-radio>
        </el-radio-group>
        <el-select
          v-if="transferMode === 'transfer'"
          v-model="targetSpace"
          placeholder="目标库"
          style="width: 100%; margin-bottom: 12px"
        >
          <el-option v-for="s in spaces" :key="s.id" :label="s.name" :value="s.id" />
        </el-select>
        <el-select v-model="transferTarget" placeholder="目标位置" style="width: 100%">
          <el-option v-for="o in locationOptions" :key="o.id" :label="o.label" :value="o.id" />
        </el-select>
        <el-text type="info" size="small" class="transfer-hint">
          库内移位只改物品所在位置；跨库转库会同时迁移所属库与在手库存。
        </el-text>
        <template #footer>
          <el-button @click="showTransfer = false">取消</el-button>
          <el-button type="primary" @click="confirmTransfer">确认</el-button>
        </template>
      </el-dialog>
    </template>
  </div>
</template>

<style scoped>
.block {
  margin-bottom: 16px;
}
.head {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.transfer-mode {
  margin-bottom: 12px;
}
.transfer-hint {
  display: block;
  margin-top: 8px;
}
</style>
