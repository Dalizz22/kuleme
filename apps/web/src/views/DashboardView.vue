<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { listSpaces } from '../api'
import { errorText } from '../api/errors'
import type { TreeNode } from '../api/types'

const spaces = ref<TreeNode[]>([])
const loading = ref(true)
const error = ref('')

async function reload() {
  loading.value = true
  error.value = ''
  try {
    spaces.value = await listSpaces()
  } catch (err) {
    error.value = errorText(err, '加载失败')
  } finally {
    loading.value = false
  }
}

onMounted(reload)
</script>

<template>
  <div v-loading="loading">
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
      <el-button size="small" @click="reload">重试</el-button>
    </el-alert>
    <el-card class="block">
      <template #header>快速入库</template>
      <router-link to="/items/new"><el-button type="primary">新建物品</el-button></router-link>
    </el-card>
    <el-card class="block">
      <template #header>仓库</template>
      <el-empty v-if="!loading && spaces.length === 0" description="暂无仓库" />
      <el-table v-else :data="spaces">
        <el-table-column prop="name" label="名称" />
        <el-table-column prop="id" label="ID" show-overflow-tooltip />
      </el-table>
    </el-card>
  </div>
</template>

<style scoped>
.block {
  margin-bottom: 16px;
}
</style>
