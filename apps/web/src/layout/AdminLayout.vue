<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listSpaces } from '../api'
import { errorText } from '../api/errors'
import type { TreeNode } from '../api/types'
import { useAuthStore } from '../stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const menus = [
  { path: '/', label: '工作台' },
  { path: '/items', label: '物品' },
  { path: '/bases', label: '基础数据' },
  { path: '/io', label: '导入导出' },
  { path: '/ai/import', label: 'AI 识别入库' },
  { path: '/recycle', label: '回收站' },
  { path: '/messages', label: '消息' },
  { path: '/settings/ai', label: '设置' },
]

const spaces = ref<TreeNode[]>([])
const spaceError = ref('')

function buildTree(nodes: TreeNode[]): TreeNode[] {
  const roots: TreeNode[] = []
  const byParent = new Map<string | null, TreeNode[]>()
  for (const n of nodes) {
    const key = n.parent_id ?? null
    if (!byParent.has(key)) byParent.set(key, [])
    byParent.get(key)!.push(n)
  }
  void byParent
  roots.push(...nodes.filter((n) => !n.parent_id).sort((a, b) => a.sort - b.sort))
  return roots
}

onMounted(async () => {
  try {
    spaces.value = buildTree(await listSpaces())
  } catch (err) {
    spaceError.value = errorText(err, '仓库列表加载失败')
  }
})

async function onLogout() {
  await auth.logout()
  ElMessage.success('已退出登录')
  await router.push({ name: 'login' })
}
</script>

<template>
  <el-container class="layout">
    <el-aside width="240px" class="sidebar">
      <div class="brand">库了么</div>
      <!-- 侧栏上半部为仓库区 -->
      <div class="space-section">
        <div class="section-title">仓库</div>
        <el-alert v-if="spaceError" :title="spaceError" type="error" :closable="false" size="small" />
        <el-empty v-else-if="spaces.length === 0" description="暂无仓库" :image-size="48" />
        <div v-else class="space-list">
          <div v-for="s in spaces" :key="s.id" class="space-item">{{ s.name }}</div>
        </div>
      </div>
      <!-- 下半部为功能菜单 -->
      <el-menu :default-active="route.path" router class="menu">
        <el-menu-item v-for="m in menus" :key="m.path" :index="m.path">{{ m.label }}</el-menu-item>
      </el-menu>
    </el-aside>
    <el-container>
      <el-header height="56px" class="topbar">
        <span class="topbar-title">{{ auth.system?.name ?? '库了么' }} 管理后台</span>
        <el-dropdown class="user">
          <span class="user-name">{{ auth.user?.display_name || auth.user?.username }}</span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item @click="onLogout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </el-header>
      <el-main class="content">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.layout {
  height: 100vh;
}
.sidebar {
  background: #fff;
  border-right: 1px solid #e4e7f0;
  display: flex;
  flex-direction: column;
}
.brand {
  height: 56px;
  display: flex;
  align-items: center;
  padding: 0 16px;
  font-size: 18px;
  font-weight: 600;
  color: var(--klm-primary);
  border-bottom: 1px solid #f0f2f8;
}
.space-section {
  padding: 8px 16px;
  border-bottom: 1px solid #f0f2f8;
  max-height: 40%;
  overflow: auto;
}
.section-title {
  font-size: 12px;
  color: #909399;
  margin-bottom: 8px;
}
.space-item {
  padding: 6px 8px;
  border-radius: 4px;
  font-size: 13px;
  color: #303133;
}
.menu {
  border-right: none;
  flex: 1;
}
.menu :deep(.el-menu-item.is-active) {
  color: var(--klm-primary);
  background: var(--el-color-primary-light-9);
  border-right: 2px solid var(--klm-primary);
}
.topbar {
  background: #fff;
  border-bottom: 1px solid #e4e7f0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.topbar-title {
  font-size: 14px;
  color: #606266;
}
.user-name {
  cursor: pointer;
  color: #303133;
  font-size: 14px;
}
.content {
  background: var(--klm-bg);
  padding: 16px;
}
</style>
