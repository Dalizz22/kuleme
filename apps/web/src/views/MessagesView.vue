<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listInbox, markMessageRead } from '../api'
import { errorText } from '../api/errors'
import type { InboxMessage } from '../api'

const items = ref<InboxMessage[]>([])
const unread = ref(0)
const loading = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const d = await listInbox()
    items.value = d.items
    unread.value = d.unread
  } catch (err) {
    error.value = errorText(err, '收件箱加载失败')
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function read(m: InboxMessage) {
  if (m.read) return
  try {
    await markMessageRead(m.id)
    m.read = true
    unread.value = Math.max(0, unread.value - 1)
  } catch (err) {
    ElMessage.error(errorText(err, '标记失败'))
  }
}
</script>

<template>
  <el-card v-loading="loading">
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
      <el-button size="small" @click="load">重试</el-button>
    </el-alert>
    <template #header>
      收件箱
      <el-badge v-if="unread" :value="unread" style="margin-left: 8px" />
    </template>
    <el-table
      :data="items"
      @row-click="read"
      :row-class-name="(r: { row: InboxMessage }) => (r.row.read ? '' : 'unread-row')"
    >
      <el-table-column width="70">
        <template #default="{ row }">
          <el-tag v-if="!row.read" type="warning" size="small">未读</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="title" label="标题" width="220" />
      <el-table-column prop="body" label="内容" show-overflow-tooltip />
      <el-table-column prop="created_at" label="时间" width="180">
        <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
      </el-table-column>
    </el-table>
    <el-empty v-if="!items.length" description="暂无消息。规则触发的通知会出现在这里" />
  </el-card>
</template>

<style>
.unread-row {
  cursor: pointer;
  font-weight: 600;
}
</style>
