<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getAiSettings, putAiSettings, testAiSettings } from '../../api'
import { errorText } from '../../api/errors'

// OpenAI 标准接口三字段：base_url + api_key + model。
// 契约矛盾：回显是 api_key_set / api_key_hint（非任务书所写 sk-**** 格式），以 server 为准。
// provider 也回填：服务端存的是 openai-compatible，写死 'openai' 会把提供者标识改坏。
const form = reactive({ provider: 'openai-compatible', base_url: '', api_key: '', model: '', enabled: true, max_images: 8, timeout_seconds: 60 })
const keyState = ref('') // 已配置时显示脱敏提示
const loading = ref(true)
const error = ref('')
const saving = ref(false)
const testing = ref(false)
const testResult = ref<{ ok: boolean; message: string; detail?: string; elapsed_ms?: number } | null>(null)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const s = await getAiSettings()
    form.provider = s.provider || 'openai-compatible'
    form.base_url = s.base_url
    form.model = s.model
    form.enabled = s.enabled
    form.max_images = s.max_images
    form.timeout_seconds = s.timeout_seconds
    keyState.value = s.api_key_set ? `已配置（${s.api_key_hint ?? '****'}）` : ''
    testResult.value = null
  } catch (err) {
    error.value = errorText(err, 'AI 设置加载失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)

async function save() {
  saving.value = true
  try {
    const body: {
      provider: string
      base_url: string
      model: string
      enabled: boolean
      max_images: number
      timeout_seconds: number
      api_key?: string
    } = {
      provider: form.provider,
      base_url: form.base_url,
      model: form.model,
      enabled: form.enabled,
      // 必须回传：服务端 omitempty + normalized 会把缺失值重置为默认，波及 Android 端图片上限。
      max_images: form.max_images,
      timeout_seconds: form.timeout_seconds,
    }
    if (form.api_key) body.api_key = form.api_key
    const s = await putAiSettings(body)
    form.api_key = ''
    keyState.value = s.api_key_set ? `已配置（${s.api_key_hint ?? '****'}）` : ''
    ElMessage.success('已保存')
  } catch (err) {
    ElMessage.error(errorText(err, '保存失败'))
  } finally {
    saving.value = false
  }
}

async function test() {
  testing.value = true
  testResult.value = null
  try {
    const r = await testAiSettings()
    testResult.value = { ok: r.ok, message: r.message, detail: r.detail, elapsed_ms: r.elapsed_ms }
  } catch (err) {
    testResult.value = { ok: false, message: errorText(err, '测试失败') }
  } finally {
    testing.value = false
  }
}
</script>

<template>
  <el-card v-loading="loading" class="card">
    <template #header>AI 设置（OpenAI 标准接口）</template>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
      <el-button size="small" @click="load">重试</el-button>
    </el-alert>
    <el-form v-else label-width="120px" @submit.prevent>
      <el-form-item label="Base URL" required>
        <el-input v-model="form.base_url" placeholder="https://api.openai.com/v1" />
        <div style="color: var(--el-text-color-secondary); font-size: 12px; margin-top: 2px">
          填到 /v1 为止（系统自动拼接 /chat/completions）。例：http://192.168.31.7:8088/v1
        </div>
      </el-form-item>
      <el-form-item label="API Key">
        <el-input v-model="form.api_key" type="password" show-password :placeholder="keyState || '输入 API Key'" />
        <div v-if="keyState" class="hint">{{ keyState }}，留空表示不修改</div>
      </el-form-item>
      <el-form-item label="模型" required>
        <el-input v-model="form.model" placeholder="例如 gpt-4o-mini" />
      </el-form-item>
      <el-form-item label="单次图片上限">
        <el-input-number v-model="form.max_images" :min="1" :max="20" />
        <div class="hint">一次识别任务最多几张图（Web 与 Android 共用）。</div>
      </el-form-item>
      <el-form-item label="超时（秒）">
        <el-input-number v-model="form.timeout_seconds" :min="5" :max="300" />
      </el-form-item>
      <el-form-item label="启用">
        <el-switch v-model="form.enabled" />
      </el-form-item>
      <el-form-item>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
        <el-button :loading="testing" @click="test">测试连接</el-button>
      </el-form-item>
      <el-form-item v-if="testResult">
        <el-alert
          :type="testResult.ok ? 'success' : 'warning'"
          :title="testResult.ok ? '连接正常' : '未配置或连接失败'"
          :description="testResult.message + (testResult.elapsed_ms != null ? `（${testResult.elapsed_ms}ms）` : '')"
          :closable="false"
        >
          <div v-if="testResult.detail" style="font-size: 12px; white-space: pre-wrap; word-break: break-all">
            {{ testResult.detail }}
          </div>
        </el-alert>
      </el-form-item>
    </el-form>
  </el-card>
</template>

<style scoped>
.card {
  max-width: 720px;
}
.block {
  margin-bottom: 16px;
}
.hint {
  font-size: 12px;
  color: #909399;
  margin-top: 4px;
}
</style>
