import { request, newIdempotencyKey, ApiError, type Page } from './request'
import type {
  AiConfirmResult,
  AiMaskedSettings,
  AiTaskResult,
  AiTestResult,
  FileObject,
  ItemView,
  PhotoLinkInput,
  PhotoRef,
  SessionResult,
  SystemInfo,
  TemplateDetail,
  TemplateManifest,
  TransferPreview,
  TreeNode,
  User,
} from './types'

// —— 系统 / 会话 ——
export const getSystemInfo = () => request<SystemInfo>('/system/info')
export const setup = (body: { username: string; password: string; display_name: string; secret: string }) =>
  request<{ user: User }>('/setup', { method: 'POST', body, retries: 0 })
export const login = (username: string, password: string) =>
  request<SessionResult>('/sessions', { method: 'POST', body: { username, password }, retries: 0 })
export const logout = () => request<null>('/sessions/current', { method: 'DELETE' })
export const getMe = () => request<User>('/me', { retries: 0 })

// —— 目录：库 / 位置 / 分类（均返回 {items}）——
// 空集合服务端可能发 null（已知 GET /locations 行为，服务端在修）；api 层兜成空数组，
// 页面不再为 null.length 白屏，也无需到处写 (x || [])。
const asList = <T,>(d: { items?: T[] | null }): T[] => (Array.isArray(d.items) ? d.items : [])
export const listSpaces = () => request<{ items: TreeNode[] }>('/spaces').then(asList)
export const listLocations = () => request<{ items: TreeNode[] }>('/locations').then(asList)
export const listCategories = () => request<{ items: TreeNode[] }>('/categories').then(asList)

// —— 模板 ——
export const listTemplates = () =>
  request<{ items: TemplateManifest[]; default_key: string }>('/templates').then((d) => ({
    ...d,
    items: Array.isArray(d.items) ? d.items : [],
  }))
export const getTemplate = (key: string) => request<TemplateDetail>(`/templates/${key}`)

// —— 物品 ——
export interface ItemListQuery {
  page: number
  page_size: number
  keyword?: string
  template_key?: string
  // 状态筛选（逗号分隔，如 in_stock,sold）；需后端 GET /items 支持 status 参数（后端部分）。
  status?: string
}
export const listItems = (q: ItemListQuery) => {
  const p = new URLSearchParams({ page: String(q.page), page_size: String(q.page_size) })
  if (q.keyword) p.set('keyword', q.keyword)
  if (q.template_key) p.set('template_key', q.template_key)
  if (q.status) p.set('status', q.status)
  return request<Page<ItemView>>(`/items?${p}`).then((d) => ({ ...d, items: Array.isArray(d.items) ? d.items : [] }))
}
export const getItem = (id: string) => request<ItemView>(`/items/${id}`)
export interface ItemCreateBody {
  space_id: string
  name: string
  template_key: string
  note?: string
  category_id?: string | null
  fields: Record<string, unknown>
  // 与建档同一事务的期初库存；逐件制数量恒 "1"，不传则物品待入库。
  initial_location_id?: string
  initial_quantity?: string
}
export const createItem = (body: ItemCreateBody, key: string) =>
  request<ItemView>('/items', { method: 'POST', body, idempotencyKey: key })
export const deleteItem = (id: string) => request<null>(`/items/${id}`, { method: 'DELETE' })

// —— 库存命令（报废/转库等；quantity 为字符串数量，serial 物品固定 "1"）——
export const inventoryOp = (
  body: {
    item_id: string
    kind: string
    location_id?: string | null
    to_location_id?: string | null
    quantity?: string
    reason?: string
  },
  key: string,
) => request('/inventory/operations', { method: 'POST', body, idempotencyKey: key })

// —— 出售 / 维修（走 /items/:id 生命周期接口；旧 POST /sales 已不再注册）——
export const saleItem = (id: string, body: { price?: string; note?: string }, key: string) =>
  request(`/items/${id}/sale`, { method: 'POST', body, idempotencyKey: key })
export const openMaintenance = (id: string, body: { reason: string; note?: string }, key: string) =>
  request(`/items/${id}/maintenance`, { method: 'POST', body, idempotencyKey: key })
export const closeMaintenance = (id: string, key: string) =>
  request(`/items/${id}/maintenance/close`, { method: 'POST', body: {}, idempotencyKey: key })

// —— 跨库转库（库内移位仍走 inventoryOp kind:'move'，服务端拒绝同库转库）——
export interface TransferBody {
  to_space_id: string
  to_location_id: string
  expected_version: number
}
export const transferPreview = (id: string, body: TransferBody) =>
  request<{ preview: TransferPreview }>(`/items/${id}/transfer-preview`, { method: 'POST', body }).then(
    (d) => d.preview,
  )
export const transferItem = (id: string, body: TransferBody, key: string) =>
  request<ItemView>(`/items/${id}/transfer`, { method: 'POST', body, idempotencyKey: key })

// —— 文件 ——
export const uploadFile = (file: File) => {
  const form = new FormData()
  form.append('file', file)
  return request<FileObject>('/files', { method: 'POST', form })
}
export const fileContentUrl = (id: string) => `/api/v1/files/${id}/content`
export const fileThumbnailUrl = (id: string) => `/api/v1/files/${id}/thumbnail`
// 只允许删「上传者本人 + 未关联」的私有文件；已关联 409 FILE_IN_USE，他人/不存在 404。
export const deleteFile = (id: string) => request<null>(`/files/${id}`, { method: 'DELETE', retries: 0 })

// —— 物品照片——
// 契约：这组接口不要求 Idempotency-Key（关联/解除/排序/设主图天然幂等或整体替换）。
// 全部返回 data {photos: [PhotoRef]}（按 sort 升序，恰好一条 is_primary=true）。
// retries: 0 —— 409 PHOTO_ALREADY_LINKED / 403 都不是瞬时失败，重试只会重复报错。
export const listItemPhotos = (id: string) => request<{ photos: PhotoRef[] }>(`/items/${id}/photos`, { retries: 0 })
export const linkItemPhotos = (id: string, body: PhotoLinkInput) =>
  request<{ photos: PhotoRef[] }>(`/items/${id}/photos`, { method: 'POST', body, retries: 0 })
export const unlinkItemPhoto = (id: string, fileId: string) =>
  request<{ photos: PhotoRef[] }>(`/items/${id}/photos/${fileId}`, { method: 'DELETE', retries: 0 })
export const reorderItemPhotos = (id: string, fileIds: string[]) =>
  request<{ photos: PhotoRef[] }>(`/items/${id}/photos/order`, {
    method: 'PUT',
    body: { file_ids: fileIds },
    retries: 0,
  })
export const setItemPrimaryPhoto = (id: string, fileId: string) =>
  request<{ photos: PhotoRef[] }>(`/items/${id}/photos/${fileId}/primary`, { method: 'PUT', retries: 0 })

// —— AI 设置 ——
export const getAiSettings = () => request<AiMaskedSettings>('/ai/settings')
// 注意：Settings 的 max_images/timeout_seconds 是 omitempty，服务端 normalized 会把 0 重置为默认值。
// 因此每次保存都必须回传当前值（页面先 GET 回填），否则一次无关保存会静默改掉 Android 端上限。
export const putAiSettings = (body: {
  provider: string
  base_url: string
  api_key?: string
  model: string
  enabled: boolean
  max_images?: number
  timeout_seconds?: number
}) => request<AiMaskedSettings>('/ai/settings', { method: 'PUT', body })
export const testAiSettings = () => request<AiTestResult>('/ai/settings/test', { method: 'POST', retries: 0 })

export { newIdempotencyKey }

// ---- 基础数据管理（空间/位置/分类 CRUD）----
export const createSpace = (name: string) =>
  request<TreeNode>('/spaces', { method: 'POST', body: { name }, idempotencyKey: newIdempotencyKey() })
export const renameSpace = (id: string, name: string, version: number) =>
  request<TreeNode>(`/spaces/${id}`, { method: 'PATCH', body: { name, version } })
export const deleteSpace = (id: string) => request<null>(`/spaces/${id}`, { method: 'DELETE' })

export const createLocation = (name: string, parentId?: string, code?: string) =>
  request<TreeNode>('/locations', {
    method: 'POST',
    body: { name, parent_id: parentId || undefined, code: code || undefined },
    idempotencyKey: newIdempotencyKey(),
  })
export const renameLocation = (id: string, name: string, version: number) =>
  request<TreeNode>(`/locations/${id}`, { method: 'PATCH', body: { name, version } })
export const deleteLocation = (id: string) => request<null>(`/locations/${id}`, { method: 'DELETE' })

export const createCategory = (name: string, parentId?: string) =>
  request<TreeNode>('/categories', {
    method: 'POST',
    body: { name, parent_id: parentId || undefined },
    idempotencyKey: newIdempotencyKey(),
  })
export const renameCategory = (id: string, name: string, version: number) =>
  request<TreeNode>(`/categories/${id}`, { method: 'PATCH', body: { name, version } })
export const deleteCategory = (id: string) => request<null>(`/categories/${id}`, { method: 'DELETE' })

// ---- 回收站 ----
export interface RecycleItem {
  id: string
  kind: string
  name: string
  space_id: string
  public_code: string
  deleted_at: string
  purge_after: string
  expired: boolean
}
export const recycleSummary = () => request<{ items: number; spaces: number }>('/recycle')
export const listRecycleItems = (page = 1, pageSize = 50) =>
  request<{ items: RecycleItem[]; total: number }>(`/recycle/items?page=${page}&page_size=${pageSize}`).then((d) => ({
    ...d,
    items: Array.isArray(d.items) ? d.items : [],
  }))
export const restoreRecycleItem = (id: string) =>
  request<null>(`/recycle/items/${id}/restore`, { method: 'POST', idempotencyKey: newIdempotencyKey() })

// ---- 站内消息 ----
export interface InboxMessage {
  id: string
  title: string
  body: string
  run_id?: string
  read: boolean
  created_at: string
}
export const listInbox = () =>
  request<{ items: InboxMessage[]; unread: number }>('/inbox').then((d) => ({
    ...d,
    items: Array.isArray(d.items) ? d.items : [],
  }))
export const markMessageRead = (id: string) =>
  request<null>(`/inbox/${id}/read`, { method: 'POST' })

// ---- 导入 ----
export type ImportMode = 'catalog' | 'stock'
export interface ImportBatch {
  id: string
  space_id: string
  template_key: string
  mapping: Record<string, string>
  headers: string[]
  status: string
  // catalog=仅导入目录；stock=同时建初始库存（位置在创建批次时固定）。
  mode: ImportMode
  location_id?: string
  total: number
  ok: number
  failed: number
  created_at: string
}
// 行结果契约：status 取批次模式本身（catalog|stock）或 failed。
export interface ImportRow {
  row_no: number
  status: 'catalog' | 'stock' | 'failed'
  item_id?: string
  public_code?: string
  error_code?: string
  message?: string
}
// POST /imports 是 multipart：服务端幂等摘要吃原始字节，FormData 的 boundary 每次随机，
// 同键重放必被判「异内容」→ 409。约定：上传每次用新键，失败直接重传；
// 可能留一个不动库存、可弃置的无害 draft 批次，不做「先查同名批次」补偿。
export const createImport = (
  file: File,
  spaceId: string,
  templateKey: string,
  mode: ImportMode,
  locationId: string | undefined,
) => {
  const form = new FormData()
  form.append('file', file)
  form.append('space_id', spaceId)
  form.append('template_key', templateKey)
  form.append('mode', mode)
  if (mode === 'stock' && locationId) form.append('location_id', locationId)
  return request<{ batch: ImportBatch; preview: ImportRow[] }>('/imports', {
    method: 'POST',
    form,
    idempotencyKey: newIdempotencyKey(),
    retries: 0,
  })
}
export const getImport = (id: string) =>
  request<{ batch: ImportBatch; rows: ImportRow[] }>(`/imports/${id}`)
export const confirmImport = (id: string, key: string) =>
  request<{ batch: ImportBatch; rows: ImportRow[] }>(`/imports/${id}/confirm`, {
    method: 'POST',
    body: {},
    idempotencyKey: key,
  })

// ---- 导出 ----
export interface ExportFile {
  id: string
  status: string
  row_count: number
  byte_size: number
  file_name: string
  created_at: string
}
export const requestExport = (templateKey?: string, keyword?: string) =>
  request<ExportFile>('/exports', {
    method: 'POST',
    body: { template_key: templateKey || '', keyword: keyword || '' },
    idempotencyKey: newIdempotencyKey(),
  })
export const listExports = () => request<{ items: ExportFile[] }>('/exports').then((d) => ({ items: Array.isArray(d.items) ? d.items : [] }))
export const exportDownloadUrl = (id: string) => `/api/v1/exports/${id}/download`
export const deleteExport = (id: string) => request<null>(`/exports/${id}`, { method: 'DELETE' })

// —— AI 识别——
// 上传走 XHR 只为了拿到真实进度与取消；请求外壳与 ApiError 和 request 保持一致。
export function uploadFileWithProgress(
  file: File,
  onProgress: (pct: number) => void,
  signal: AbortSignal,
): Promise<FileObject> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/api/v1/files')
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(Math.round((e.loaded / e.total) * 100))
    }
    xhr.onload = () => {
      let body: { data?: FileObject; error?: { code: string; message: string; details?: Record<string, unknown> } } = {}
      try {
        body = JSON.parse(xhr.responseText || '{}')
      } catch {
        // 非 JSON 响应按状态码兜底
      }
      if (xhr.status >= 200 && xhr.status < 300 && body.data) resolve(body.data)
      else reject(new ApiError(xhr.status, body.error?.code ?? '', body.error?.message ?? '上传失败', body.error?.details ?? {}))
    }
    xhr.onerror = () => reject(new TypeError('network error'))
    xhr.onabort = () => reject(new DOMException('上传已取消', 'AbortError'))
    signal.addEventListener('abort', () => xhr.abort(), { once: true })
    const form = new FormData()
    form.append('file', file)
    xhr.send(form)
  })
}

export const createAiTask = (
  body: { space_id: string; file_ids: string[]; template_key?: string; location_id?: string },
  key: string,
) => request<AiTaskResult>('/ai/tasks', { method: 'POST', body, idempotencyKey: key, retries: 0 })

export const getAiTask = (id: string) => request<AiTaskResult>(`/ai/tasks/${id}`, { retries: 0 })

// 确认语义：photo_ids 传 null（且 share_photos=true）= 共享全部来源图；传 [] = 明确不关联。
export const confirmAiCandidate = (
  candidateId: string,
  body: {
    space_id: string
    location_id?: string
    name?: string
    fields?: Record<string, unknown>
    quantity?: string
    photo_ids: string[] | null
    share_photos?: boolean
  },
  key: string,
) => request<AiConfirmResult>(`/ai/candidates/${candidateId}/confirm`, { method: 'POST', body, idempotencyKey: key, retries: 0 })
