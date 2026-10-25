<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { listItems } from '../../api'
import { errorText } from '../../api/errors'
import type { ItemView } from '../../api/types'

const router = useRouter()

const route = useRoute()

// 服务端派生状态全集（server/internal/item/service.go itemStatuses，与 viewSQL CASE 逐字一致）。
// 筛选走 GET /items?status=a,b，空 = 全部；total 是过滤后的数量。
const STATUS_OPTIONS = [
  { value: 'in_stock', label: '在库' },
  { value: 'pending_stock', label: '待入库' },
  { value: 'reserved', label: '预留' },
  { value: 'maintenance', label: '维修中' },
  { value: 'sold', label: '已售' },
  { value: 'scrapped', label: '已报废' },
  { value: 'lost', label: '已遗失' },
]

// 筛选/分页/关键词放 URL query：从详情返回、刷新、前进后退都能保持。
function parseStatuses(): string[] {
  const raw = route.query.status
  return (Array.isArray(raw) ? raw.join(',') : (raw ?? ''))
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

const query = reactive({
  page: Math.max(1, Number(route.query.page) || 1),
  page_size: 20,
  keyword: typeof route.query.keyword === 'string' ? route.query.keyword : '',
  statuses: parseStatuses(),
})
const rows = ref<ItemView[]>([])
const total = ref(0)
const loading = ref(false)
const error = ref('')

function statusLabel(s: string): string {
  return STATUS_OPTIONS.find((o) => o.value === s)?.label ?? s
}
function statusType(s: string): 'success' | 'warning' | 'info' | 'danger' {
  if (s === 'in_stock') return 'success'
  if (s === 'pending_stock') return 'info'
  if (s === 'reserved' || s === 'maintenance') return 'warning'
  return 'danger'
}

const hasFilter = computed(() => !!query.keyword || query.statuses.length > 0)

function syncQuery() {
  void router.replace({
    query: {
      ...(query.keyword ? { keyword: query.keyword } : {}),
      ...(query.statuses.length ? { status: query.statuses.join(',') } : {}),
      ...(query.page > 1 ? { page: String(query.page) } : {}),
    },
  })
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const page = await listItems({
      page: query.page,
      page_size: query.page_size,
      keyword: query.keyword || undefined,
      status: query.statuses.length ? query.statuses.join(',') : undefined,
    })
    // 服务端已按 status 过滤并给出过滤后的 total，不再对当前页二次过滤。
    rows.value = page.items
    total.value = page.total
    syncQuery()
  } catch (err) {
    error.value = errorText(err, '物品列表加载失败')
    rows.value = []
    total.value = 0
  } finally {
    loading.value = false
  }
}

function onSearch() {
  query.page = 1
  void load()
}

onMounted(load)

function openDetail(row: ItemView) {
  void router.push({ name: 'item-detail', params: { id: row.id } })
}

function onPageChange(p: number) {
  query.page = p
  void load()
}
</script>

<template>
  <div>
    <el-card>
      <div class="toolbar">
        <el-select
          v-model="query.statuses"
          multiple
          collapse-tags
          placeholder="状态"
          class="status-filter"
          @change="onSearch"
        >
          <el-option v-for="o in STATUS_OPTIONS" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
        <el-input
          v-model="query.keyword"
          placeholder="搜索名称 / 编码"
          clearable
          class="search"
          @keyup.enter="onSearch"
          @clear="onSearch"
        />
        <el-button type="primary" @click="onSearch">搜索</el-button>
        <router-link to="/items/new"><el-button>新建物品</el-button></router-link>
      </div>

      <el-alert v-if="error" :title="error" type="error" :closable="false" class="block">
        <el-button size="small" @click="load">重试</el-button>
      </el-alert>

      <el-table v-loading="loading" :data="rows" @row-click="openDetail">
        <el-table-column prop="public_code" label="编码" width="140" />
        <el-table-column prop="name" label="名称" min-width="180" show-overflow-tooltip />
        <el-table-column prop="location_path" label="位置" min-width="160" show-overflow-tooltip>
          <template #default="{ row }">{{ row.location_path || '—' }}</template>
        </el-table-column>
        <el-table-column prop="quantity" label="数量" width="90" />
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small">{{ statusLabel(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="updated_at" label="更新时间" width="180">
          <template #default="{ row }">{{ new Date(row.updated_at).toLocaleString('zh-CN') }}</template>
        </el-table-column>
        <template #empty>
          <el-empty :description="hasFilter ? '没有符合筛选条件的物品' : '暂无物品'" />
        </template>
      </el-table>

      <div class="pager">
        <el-pagination
          background
          layout="total, prev, pager, next"
          :total="total"
          :page-size="query.page_size"
          :current-page="query.page"
          @current-change="onPageChange"
        />
      </div>
      <div class="note">状态筛选在服务端完成（status=逗号分隔），分页与总数均为过滤后结果；清空筛选即全部。</div>
    </el-card>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  gap: 8px;
  margin-bottom: 16px;
}
.status-filter {
  width: 240px;
}
.search {
  width: 280px;
}
.block {
  margin-bottom: 16px;
}
.pager {
  margin-top: 16px;
  display: flex;
  justify-content: flex-end;
}
.note {
  margin-top: 8px;
  font-size: 12px;
  color: #909399;
}
</style>
