<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '../stores/auth'
import { errorText } from '../api/errors'
import { ApiError } from '../api/request'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const form = reactive({ username: '', password: '' })
const loading = ref(false)
const errorMsg = ref('')

async function submit() {
  if (!form.username || !form.password) {
    errorMsg.value = '请输入用户名和密码'
    return
  }
  loading.value = true
  errorMsg.value = ''
  try {
    await auth.login(form.username, form.password)
    ElMessage.success('登录成功')
    await router.push((route.query.redirect as string) || '/')
  } catch (err) {
    // 登录页的 401 是「账号或密码错误」，不是会话失效；errorText 的 401 文案面向已登录页面，这里用服务端原文。
    errorMsg.value =
      err instanceof ApiError && err.status === 401
        ? err.message || '账号或密码错误'
        : errorText(err, '登录失败，请稍后重试')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-page">
    <el-card class="login-card">
      <h1 class="title">库了么 · 管理后台</h1>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="用户名">
          <el-input v-model="form.username" autocomplete="username" placeholder="请输入用户名" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input
            v-model="form.password"
            type="password"
            autocomplete="current-password"
            show-password
            placeholder="请输入密码"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-alert v-if="errorMsg" :title="errorMsg" type="error" :closable="false" class="error" />
        <el-button type="primary" class="submit" :loading="loading" @click="submit">登 录</el-button>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--klm-bg);
}
.login-card {
  width: 400px;
}
.title {
  margin: 0 0 24px;
  font-size: 20px;
  color: var(--klm-primary);
  text-align: center;
}
.error {
  margin-bottom: 16px;
}
.submit {
  width: 100%;
}
</style>
