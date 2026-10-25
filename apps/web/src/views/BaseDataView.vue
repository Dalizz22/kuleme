<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  listSpaces,
  listLocations,
  listCategories,
  createSpace,
  renameSpace,
  deleteSpace,
  createLocation,
  renameLocation,
  deleteLocation,
  createCategory,
  renameCategory,
  deleteCategory,
} from '../api'
import { errorText } from '../api/errors'
import type { TreeNode } from '../api/types'

const tab = ref<'spaces' | 'locations' | 'categories'>('spaces')
const loading = ref(false)

// 空间
const spaces = ref<TreeNode[]>([])
const spaceDlg = ref(false)
const spaceName = ref('')
const spaceEditing = ref<TreeNode | null>(null)

// 位置 / 分类：树形统一处理
const locations = ref<TreeNode[]>([])
const categories = ref<TreeNode[]>([])
const treeDlg = ref(false)
const treeDlgTitle = ref('')
const nodeName = ref('')
const nodeCode = ref('')
const nodeParent = ref<TreeNode | null>(null)
const nodeEditing = ref<TreeNode | null>(null)

function msg(err: unknown, fallback: string): string {
  return errorText(err, fallback)
}

async function load() {
  loading.value = true
  try {
    const [s, l, c] = await Promise.all([listSpaces(), listLocations(), listCategories()])
    spaces.value = s
    locations.value = l
    categories.value = c
  } catch (err) {
    ElMessage.error(msg(err, '基础数据加载失败'))
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function addSpace() {
  if (!spaceName.value.trim()) return
  try {
    if (spaceEditing.value) await renameSpace(spaceEditing.value.id, spaceName.value.trim(), spaceEditing.value.version)
    else await createSpace(spaceName.value.trim())
    spaceDlg.value = false
    spaceName.value = ''
    ElMessage.success('仓库已创建')
    await load()
  } catch (err) {
    ElMessage.error(msg(err, '创建失败'))
  }
}

function openCreate(parent: TreeNode | null, editing: TreeNode | null) {
  nodeParent.value = parent
  nodeEditing.value = editing
  nodeName.value = editing?.name ?? ''
  nodeCode.value = ''
  treeDlgTitle.value = editing
    ? `重命名「${editing.name}」`
    : parent
      ? `在「${parent.name}」下新建${tab.value === 'locations' ? '位置' : '分类'}`
      : `新建根${tab.value === 'locations' ? '位置' : '分类'}`
  treeDlg.value = true
}

async function saveNode() {
  const name = nodeName.value.trim()
  if (!name) return
  const list = tab.value === 'locations' ? locations : categories
  try {
    if (nodeEditing.value) {
      if (tab.value === 'locations') await renameLocation(nodeEditing.value.id, name, nodeEditing.value.version)
      else await renameCategory(nodeEditing.value.id, name, nodeEditing.value.version)
    } else if (tab.value === 'locations') {
      await createLocation(name, nodeParent.value?.id, nodeCode.value.trim() || undefined)
    } else {
      await createCategory(name, nodeParent.value?.id)
    }
    treeDlg.value = false
    ElMessage.success('已保存')
    await load()
    void list
  } catch (err) {
    ElMessage.error(msg(err, '保存失败'))
  }
}

async function removeSpace(row: TreeNode) {
  try {
    await ElMessageBox.confirm(`删除仓库「${row.name}」？物品不为空时会被拒绝；删除后 30 天内可在回收站恢复。`, '确认删除', { type: 'warning' })
  } catch {
    return
  }
  try {
    await deleteSpace(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(msg(err, '删除失败'))
  }
}

async function removeNode(row: TreeNode) {
  try {
    await ElMessageBox.confirm(`删除「${row.name}」？`, '确认删除', { type: 'warning' })
  } catch {
    return
  }
  try {
    if (tab.value === 'locations') await deleteLocation(row.id)
    else await deleteCategory(row.id)
    ElMessage.success('已删除')
    await load()
  } catch (err) {
    ElMessage.error(msg(err, '删除失败'))
  }
}
</script>

<template>
  <el-card v-loading="loading">
    <template #header>基础数据</template>
    <el-tabs v-model="tab">
      <el-tab-pane label="仓库" name="spaces">
        <el-button type="primary" @click="spaceEditing = null; spaceName = ''; spaceDlg = true">新建仓库</el-button>
        <el-table :data="spaces" style="margin-top: 12px">
          <el-table-column prop="name" label="名称" />
          <el-table-column label="操作" width="160">
            <template #default="{ row }">
              <el-button size="small" @click="spaceEditing = row; spaceName = row.name; spaceDlg = true">重命名</el-button>
              <el-button size="small" type="danger" @click="removeSpace(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-empty v-if="!spaces.length" description="还没有仓库，点「新建仓库」开始" />
      </el-tab-pane>

      <el-tab-pane label="位置" name="locations">
        <el-button type="primary" @click="openCreate(null, null)">新建根位置</el-button>
        <el-tree
          :data="locations"
          :props="{ label: 'name', children: 'children' }"
          default-expand-all
          style="margin-top: 12px"
        >
          <template #default="{ data }">
            <span style="flex: 1; display: inline-flex; gap: 8px; align-items: center">
              {{ data.name }}
              <el-button size="small" text type="primary" @click.stop="openCreate(data, null)">+子级</el-button>
              <el-button size="small" text @click.stop="openCreate(null, data)">重命名</el-button>
              <el-button size="small" text type="danger" @click.stop="removeNode(data)">删除</el-button>
            </span>
          </template>
        </el-tree>
        <el-empty v-if="!locations.length" description="还没有位置，先建根位置，再到物品上选择" />
      </el-tab-pane>

      <el-tab-pane label="分类" name="categories">
        <el-button type="primary" @click="openCreate(null, null)">新建根分类</el-button>
        <el-tree
          :data="categories"
          :props="{ label: 'name', children: 'children' }"
          default-expand-all
          style="margin-top: 12px"
        >
          <template #default="{ data }">
            <span style="flex: 1; display: inline-flex; gap: 8px; align-items: center">
              {{ data.name }}
              <el-button size="small" text type="primary" @click.stop="openCreate(data, null)">+子级</el-button>
              <el-button size="small" text @click.stop="openCreate(null, data)">重命名</el-button>
              <el-button size="small" text type="danger" @click.stop="removeNode(data)">删除</el-button>
            </span>
          </template>
        </el-tree>
        <el-empty v-if="!categories.length" description="还没有分类" />
      </el-tab-pane>
    </el-tabs>

    <el-dialog v-model="spaceDlg" :title="spaceEditing ? '重命名仓库' : '新建仓库'" width="420px">
      <el-input v-model="spaceName" placeholder="仓库名称，例如：总仓" @keyup.enter="addSpace" />
      <template #footer>
        <el-button @click="spaceDlg = false">取消</el-button>
        <el-button type="primary" @click="addSpace">保存</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="treeDlg" :title="treeDlgTitle" width="460px">
      <el-input v-model="nodeName" placeholder="名称" @keyup.enter="saveNode" />
      <el-input
        v-if="tab === 'locations' && !nodeEditing"
        v-model="nodeCode"
        placeholder="位置码（可选，10 位，留空自动）"
        style="margin-top: 8px"
      />
      <template #footer>
        <el-button @click="treeDlg = false">取消</el-button>
        <el-button type="primary" @click="saveNode">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>
