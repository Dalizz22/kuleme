<script setup lang="ts">
// 物品照片面板（Web）：上传 → 关联 → 列表/预览 → 删除关联 → 设主图 → 排序。
// 服务端事实（apps/web/src/api 里已封装）：
// - 关联/解除/排序/设主图都返回 data {photos: [PhotoRef]}（sort 升序，恰好一条 is_primary）；
// - 这组接口不要求 Idempotency-Key，失败不留半关联；
// - 解除关联不删文件（退回上传者私有），删除文件是另一条链路。
// 客户端纪律：不猜成功（一切以响应里的 photos 为准）、不永久转圈（finally 复位）、
// 上传成功但关联失败要保留 fileId 让用户重试关联，不静默丢。
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  deleteFile,
  fileContentUrl,
  fileThumbnailUrl,
  linkItemPhotos,
  listItemPhotos,
  reorderItemPhotos,
  setItemPrimaryPhoto,
  unlinkItemPhoto,
  uploadFile,
} from '../api'
import { ApiError } from '../api/request'
import { errorText } from '../api/errors'
import type { PhotoRef } from '../api/types'

const props = defineProps<{
  itemId: string
  /** GET /items/:id 的 photo_file_id（主图）。仅用于照片列表接口不可用时的降级展示。 */
  primaryFileId?: string | null
}>()
const emit = defineEmits<{ updated: [] }>()

const photos = ref<PhotoRef[]>([])
const loading = ref(false)
const uploading = ref(false)
/** 上传成功但未关联的 fileId：网络/冲突失败后保留，重试时复用，不重复上传。 */
const orphanIds = ref<string[]>([])
/** 抽屉里的卡片列表接口是否可用（当前服务端没有 GET，降级为主图单张）。 */
const listUnavailable = ref(false)
const failedReason = ref('')

const sorted = computed(() => [...photos.value].sort((a, b) => a.sort - b.sort))
const primary = computed(() => photos.value.find((p) => p.is_primary) ?? null)

function apply(list: PhotoRef[] | undefined) {
  if (Array.isArray(list)) photos.value = list
}

/** 只有 NOT_IMPLEMENTED 才是「接口未上线」；404 是真实错误（物品不存在/无权访问），要显示原因。 */
function isMissingEndpoint(e: unknown) {
  return e instanceof ApiError && e.code === 'NOT_IMPLEMENTED'
}

async function load() {
  loading.value = true
  failedReason.value = ''
  try {
    const res = await listItemPhotos(props.itemId)
    apply(res.photos)
    listUnavailable.value = false
  } catch (e) {
    if (isMissingEndpoint(e)) {
      listUnavailable.value = true
      photos.value = props.primaryFileId
        ? [{ file_id: props.primaryFileId, is_primary: true, sort: 1, url: fileContentUrl(props.primaryFileId) }]
        : []
    } else {
      failedReason.value = errorText(e, '照片加载失败')
    }
  } finally {
    loading.value = false
  }
}

/** 关联一组已上传的 fileId；firstAsPrimary 仅在没有主图时生效。 */
async function link(fileIds: string[], firstAsPrimary: boolean) {
  const res = await linkItemPhotos(props.itemId, {
    file_ids: fileIds,
    ...(firstAsPrimary && !primary.value ? { primary_file_id: fileIds[0] } : {}),
  })
  orphanIds.value = []
  apply(res.photos)
  emit('updated')
}

async function onPick(uploadFileObj: { raw?: File }) {
  const file = uploadFileObj.raw
  if (!file) return
  uploading.value = true
  failedReason.value = ''
  try {
    const obj = await uploadFile(file)
    orphanIds.value = [...orphanIds.value, obj.id]
  } catch (e) {
    // 413/415 是文件本身的问题，提示后可重选；不属于「不可丢」的半成品。
    failedReason.value = `「${file.name}」上传失败：${errorText(e, '上传失败')}`
  } finally {
    uploading.value = false
  }
}

/** 把 orphanIds 关联到当前物品（全部上传成功后一次性提交，主图取缺失时的第一张）。 */
async function linkOrphans() {
  if (orphanIds.value.length === 0) return
  uploading.value = true
  failedReason.value = ''
  try {
    await link([...orphanIds.value], true)
    ElMessage.success('照片已关联')
  } catch (e) {
    // 409 PHOTO_ALREADY_LINKED / 403 文件不属于该库：保留 orphanIds，用户可删掉或换一张重试。
    failedReason.value = `关联失败（文件已保留，可重试）：${errorText(e, '关联失败')}`
  } finally {
    uploading.value = false
  }
}

async function setPrimary(p: PhotoRef) {
  if (p.is_primary) return
  uploading.value = true
  try {
    const res = await setItemPrimaryPhoto(props.itemId, p.file_id)
    apply(res.photos)
    emit('updated')
  } catch (e) {
    failedReason.value = errorText(e, '设为主图失败')
  } finally {
    uploading.value = false
  }
}

async function move(p: PhotoRef, delta: -1 | 1) {
  const list = sorted.value.map((x) => x.file_id)
  const i = list.indexOf(p.file_id)
  const j = i + delta
  if (i < 0 || j < 0 || j >= list.length) return
  ;[list[i], list[j]] = [list[j], list[i]]
  uploading.value = true
  try {
    // 排序是整体替换：必须提交完整集合，服务端缺项/多项会 422。
    const res = await reorderItemPhotos(props.itemId, list)
    apply(res.photos)
  } catch (e) {
    failedReason.value = errorText(e, '排序失败')
  } finally {
    uploading.value = false
  }
}

async function unlink(p: PhotoRef) {
  try {
    await ElMessageBox.confirm(
      p.is_primary ? '这是主图，解除后服务端会指定下一张为主图。确定解除关联？' : '解除关联后文件不会被删除，仍可在上传者私有区找到。确定解除？',
      '解除照片关联',
      { type: 'warning', confirmButtonText: '解除关联', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  uploading.value = true
  try {
    const res = await unlinkItemPhoto(props.itemId, p.file_id)
    apply(res.photos)
    emit('updated')
  } catch (e) {
    failedReason.value = errorText(e, '解除关联失败')
  } finally {
    uploading.value = false
  }
}

/** 放弃这批未关联上传：尽力 DELETE /files/:id 清理，失败只提示不阻断（服务端只允许上传者删未关联文件）。 */
async function dropOrphans() {
  const ids = [...orphanIds.value]
  uploading.value = true
  failedReason.value = ''
  try {
    const results = await Promise.allSettled(ids.map((f) => deleteFile(f)))
    orphanIds.value = []
    const left = results.filter((r) => r.status === 'rejected').length
    if (left > 0) failedReason.value = `已放弃 ${ids.length} 张，其中 ${left} 张未能删除（可能已关联或不属于本人）`
  } finally {
    uploading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="photo-panel">
    <div class="toolbar">
      <el-upload
        :auto-upload="false"
        :show-file-list="false"
        :disabled="uploading"
        accept="image/*"
        multiple
        :on-change="onPick"
      >
        <el-button type="primary" :loading="uploading">选择图片</el-button>
      </el-upload>
      <el-button v-if="orphanIds.length" type="success" :loading="uploading" @click="linkOrphans">
        关联已选 {{ orphanIds.length }} 张
      </el-button>
      <el-button v-if="orphanIds.length" :disabled="uploading" @click="dropOrphans">放弃这批</el-button>
      <el-text type="info" size="small">单张受服务端大小与格式限制；上传成功后点「关联」才会归属本物品。</el-text>
    </div>

    <el-alert v-if="failedReason" type="error" :title="failedReason" show-icon closable @close="failedReason = ''" />
    <el-alert
      v-if="listUnavailable"
      type="warning"
      show-icon
      :closable="false"
      title="服务端暂未提供照片列表接口，这里只显示主图；上传、设主图、解除关联仍可用（以每次响应为准）。"
    />

    <el-skeleton v-if="loading" :rows="2" animated />
    <div v-else class="grid">
      <div v-for="(p, i) in sorted" :key="p.file_id" class="cell">
        <el-image
          :src="fileThumbnailUrl(p.file_id)"
          :preview-src-list="[fileContentUrl(p.file_id)]"
          fit="cover"
          class="thumb"
          preview-teleported
        />
        <el-tag v-if="p.is_primary" type="success" size="small" effect="dark" class="primary-tag">主图</el-tag>
        <div class="actions">
          <el-button size="small" link type="primary" :disabled="uploading || p.is_primary" @click="setPrimary(p)">
            设为主图
          </el-button>
          <el-button size="small" link :disabled="uploading || i === 0" @click="move(p, -1)">前移</el-button>
          <el-button size="small" link :disabled="uploading || i === sorted.length - 1" @click="move(p, 1)">后移</el-button>
          <el-button size="small" link type="danger" :disabled="uploading" @click="unlink(p)">解除</el-button>
        </div>
      </div>
      <el-empty v-if="sorted.length === 0" description="暂无照片" :image-size="64" />
    </div>
  </div>
</template>

<style scoped>
.photo-panel { display: flex; flex-direction: column; gap: 12px; }
.toolbar { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.grid { display: flex; flex-wrap: wrap; gap: 12px; }
.cell { position: relative; width: 148px; display: flex; flex-direction: column; gap: 4px; }
.thumb { width: 148px; height: 148px; border-radius: 6px; background: var(--el-fill-color-light); }
.primary-tag { position: absolute; top: 6px; left: 6px; }
.actions { display: flex; flex-wrap: wrap; gap: 2px; }
.actions :deep(.el-button + .el-button) { margin-left: 0; }
</style>
