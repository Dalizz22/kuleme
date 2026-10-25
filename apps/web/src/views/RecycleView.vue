<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listRecycleItems, restoreRecycleItem } from '../api'
import { errorText } from '../api/errors'
import type { RecycleItem } from '../api'

const items = ref<RecycleItem[]>([])
const total = ref(0)
const page = ref(1)
const loading = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const d = await listRecycleItems(page.value)
    items.value = d.items
    total.value = d.total
  } catch (err) {
    error.value = errorText(err, '回收站加载失败')
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function restore(row: RecycleItem) {
  try {
    await restoreRecycleItem(row.id)
    ElMessage.success(`「${row.name}」已恢复`)
    await load()
  } catch (err) {
    ElMessage.error(errorText(err, '恢复失败'))
  }
}
</script>

<template>
  <el-card v-loading="loading">
    <template #header>回收站（删除后保留 30 天）</template>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
      <el-button size="small" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items">
      <el-table-column prop="name" label="名称" />
      <el-table-column prop="public_code" label="编码" width="140" />
      <el-table-column label="删除时间" width="180">
        <template #default="{ row }">{{ new Date(row.deleted_at).toLocaleString() }}</template>
      </el-table-column>
      <el-table-column label="保留至" width="180">
        <template #default="{ row }">
          {{ new Date(row.purge_after).toLocaleDateString() }}
          <el-tag v-if="row.expired" type="danger" size="small" style="margin-left: 4px">已过期</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="120">
        <template #default="{ row }">
          <el-button
            size="small"
            type="primary"
            :disabled="row.expired"
            @click="restore(row)"
          >
            恢复
          </el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-pagination
      v-if="total > 50"
      v-model:current-page="page"
      :page-size="50"
      :total="total"
      layout="prev, pager, next"
      style="margin-top: 12px"
      @current-change="load"
    />
    <el-empty v-if="!items.length" description="回收站是空的" />
  </el-card>
</template>
