<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { setup } from '../api'
import { errorText } from '../api/errors'
import { useAuthStore } from '../stores/auth'

const router = useRouter()
const auth = useAuthStore()

const form = reactive({ username: '', password: '', confirm: '', display_name: '' })
const loading = ref(false)
const errorMsg = ref('')

async function submit() {
  errorMsg.value = ''
  if (!form.username || !form.password) {
    errorMsg.value = '用户名和密码必填'
    return
  }
  if (form.password !== form.confirm) {
    errorMsg.value = '两次输入的密码不一致'
    return
  }
  loading.value = true
  try {
    // secret 留空 = 网页向导初始化（未初始化时首访问者可抢先）
    await setup({
      username: form.username,
      password: form.password,
      display_name: form.display_name || form.username,
      secret: '',
    })
    ElMessage.success('初始化完成，请登录')
    auth.system = { ...(auth.system as NonNullable<typeof auth.system>), initialized: true }
    await router.push({ name: 'login' })
  } catch (err) {
    errorMsg.value = errorText(err, '初始化失败，请稍后重试')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="setup-page">
    <el-card class="setup-card">
      <h1 class="title">初始化 库了么</h1>
      <p class="hint">首次使用，请创建系统管理员账号。</p>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="用户名" required>
          <el-input v-model="form.username" autocomplete="username" />
        </el-form-item>
        <el-form-item label="显示名称">
          <el-input v-model="form.display_name" placeholder="默认同用户名" />
        </el-form-item>
        <el-form-item label="密码" required>
          <el-input v-model="form.password" type="password" autocomplete="new-password" show-password />
        </el-form-item>
        <el-form-item label="确认密码" required>
          <el-input v-model="form.confirm" type="password" autocomplete="new-password" show-password />
        </el-form-item>
        <el-alert v-if="errorMsg" :title="errorMsg" type="error" :closable="false" class="error" />
        <el-button type="primary" class="submit" :loading="loading" @click="submit">完成初始化</el-button>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.setup-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--klm-bg);
}
.setup-card {
  width: 420px;
}
.title {
  margin: 0 0 8px;
  font-size: 20px;
  color: var(--klm-primary);
}
.hint {
  margin: 0 0 16px;
  color: #909399;
  font-size: 13px;
}
.error {
  margin-bottom: 16px;
}
.submit {
  width: 100%;
}
</style>
