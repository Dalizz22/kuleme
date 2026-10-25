<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  createItem,
  getTemplate,
  listLocations,
  listSpaces,
  listTemplates,
  newIdempotencyKey,
} from '../../api'
import { errorText, recoverFailedWrite } from '../../api/errors'
import type { ItemView, TemplateDetail, TemplateManifest, TreeNode } from '../../api/types'
import DynamicFieldForm from '../../components/DynamicFieldForm.vue'

const router = useRouter()

const spaces = ref<TreeNode[]>([])
const locations = ref<TreeNode[]>([])
const templates = ref<TemplateManifest[]>([])
const tplDetail = ref<TemplateDetail | null>(null)

const form = reactive({
  space_id: '',
  location_id: '',
  template_key: '',
  name: '',
  note: '',
})
const fields = ref<Record<string, unknown>>({})

const loadingMeta = ref(true)
const metaError = ref('')
const submitting = ref(false)

// 幂等键：同一份内容失败重试复用；内容改了就是新动作，换新键避免同键异内容 409。
let idemKey = newIdempotencyKey()
let lastPayload = ''

function buildPayload() {
  return {
    space_id: form.space_id,
    name: form.name,
    template_key: form.template_key,
    note: form.note || undefined,
    fields: fields.value,
    // 建档与期初入库一次 POST 同事务完成，数量恒为 1。
    ...(form.location_id ? { initial_location_id: form.location_id, initial_quantity: '1' } : {}),
  }
}

async function loadMeta() {
  loadingMeta.value = true
  metaError.value = ''
  try {
    const [sp, loc, tpls] = await Promise.all([listSpaces(), listLocations(), listTemplates()])
    spaces.value = sp
    locations.value = loc
    templates.value = tpls.items
    if (!form.template_key && tpls.default_key) form.template_key = tpls.default_key
  } catch (err) {
    metaError.value = errorText(err, '基础数据加载失败')
  } finally {
    loadingMeta.value = false
  }
}

onMounted(loadMeta)

watch(
  () => form.template_key,
  async (key) => {
    tplDetail.value = null
    fields.value = {}
    if (!key) return
    try {
      tplDetail.value = await getTemplate(key)
    } catch (err) {
      ElMessage.error(errorText(err, '模板加载失败'))
    }
  },
  { immediate: true },
)

async function submit() {
  if (!form.space_id || !form.name || !form.template_key) {
    ElMessage.warning('库、名称、模板为必填项')
    return
  }
  submitting.value = true
  const body = buildPayload()
  const snapshot = JSON.stringify(body)
  if (snapshot !== lastPayload) {
    idemKey = newIdempotencyKey()
    lastPayload = snapshot
  }
  try {
    const item = await createItem(body, idemKey)
    ElMessage.success('创建成功')
    await router.push({ name: 'item-detail', params: { id: item.id } })
  } catch (err) {
    // 失败重试复用同一 idemKey（内容未变时）；响应丢失先按该键查终态，不换键重提。
    const r = await recoverFailedWrite<ItemView>(idemKey, err, '创建失败')
    if (r.recovered && r.result) {
      ElMessage.success('创建已生效，正在打开详情')
      await router.push({ name: 'item-detail', params: { id: r.result.id } })
      return
    }
    ElMessage.error(r.message)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-card v-loading="loadingMeta">
    <template #header>新建物品</template>
    <el-alert v-if="metaError" :title="metaError" type="error" :closable="false" class="block">
      <el-button size="small" @click="loadMeta">重试</el-button>
    </el-alert>
    <el-empty v-else-if="!loadingMeta && spaces.length === 0" description="暂无仓库，请先在服务端创建仓库" />
    <el-form v-else label-width="96px" class="form" @submit.prevent="submit">
      <el-form-item label="库" required>
        <el-select v-model="form.space_id" placeholder="选择库">
          <el-option v-for="s in spaces" :key="s.id" :label="s.name" :value="s.id" />
        </el-select>
      </el-form-item>
      <el-form-item label="位置">
        <el-select v-model="form.location_id" placeholder="选择位置（可选；选了即入库 1 件）" clearable>
          <el-option v-for="l in locations" :key="l.id" :label="l.name" :value="l.id" />
        </el-select>
      </el-form-item>
      <el-form-item label="模板" required>
        <el-select v-model="form.template_key" placeholder="选择模板">
          <el-option v-for="t in templates" :key="t.template_key" :label="t.name" :value="t.template_key" />
        </el-select>
      </el-form-item>
      <el-form-item label="名称" required>
        <el-input v-model="form.name" maxlength="120" />
      </el-form-item>
      <el-form-item label="备注">
        <el-input v-model="form.note" type="textarea" :rows="2" maxlength="500" />
      </el-form-item>
      <DynamicFieldForm v-if="tplDetail" v-model="fields" :fields="tplDetail.manifest.fields" />
      <el-form-item>
        <el-button type="primary" :loading="submitting" @click="submit">创建</el-button>
        <el-button @click="router.back()">取消</el-button>
      </el-form-item>
    </el-form>
  </el-card>
</template>

<style scoped>
.form {
  max-width: 640px;
}
.block {
  margin-bottom: 16px;
}
</style>
