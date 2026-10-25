// 与 server/ 真实代码对齐的 DTO。字段名以 Go json tag 为准。
export interface SystemInfo {
  instance_id: string
  name: string
  api_version: string
  initialized: boolean
}

export interface User {
  id: string
  username: string
  display_name: string
  is_admin: boolean
}

export interface SessionResult {
  token: string
  user: User
}

export interface TreeNode {
  id: string
  parent_id: string | null
  name: string
  sort: number
  version: number
  kind?: string
}

export interface ItemView {
  id: string
  template_key: string
  name: string
  public_code: string
  location_id: string | null
  location_path: string
  quantity: string
  status: string
  // 生命周期字段（server item.View 已有）：派生 status 的依据。
  lifecycle: string
  for_sale: boolean
  maintenance_open: boolean
  version: number
  created_at: string
  updated_at: string
  photo_file_id: string | null
  note?: string
  fields: Record<string, unknown>
  // 库级动作可用性。只表达权限/功能开关，不表达业务态；缺键/null 一律视为可用。
  // GET /items 列表不填这两项（服务端返回 null），只有 GET /items/:id 有值 → 必须可空。
  allowed_actions?: Record<string, boolean> | null
  action_blocked_reasons?: Record<string, string> | null
}

// 转库干跑结果（server/internal/item/transfer.go TransferPreview）。
export interface TransferPreview {
  item_id: string
  item_name: string
  from_space_id: string
  from_location_id: string
  to_space_id: string
  to_location_id: string
  warnings: string[]
}

export interface TemplateField {
  field_id: string
  label: string
  type: 'text' | 'textarea' | 'number' | 'money' | 'date' | 'bool' | 'enum'
  required?: boolean
  default?: unknown
  options?: string[]
  max_len?: number
  help?: string
  readonly?: boolean
}

export interface TemplateManifest {
  schema_version: number
  template_key: string
  name: string
  description?: string
  management: string
  unit?: string
  is_default?: boolean
  fields: TemplateField[]
}

export interface TemplateDetail {
  manifest: TemplateManifest
  labels: Record<string, string>
}

export interface FileObject {
  id: string
  file_name: string
  content_type: string
  byte_size: number
  created_at: string
}

// 照片引用（server/internal/item/photos.go photoView = openapi PhotoRef）。
// url 是受控内容地址 /api/v1/files/{id}/content；缩略图另走 /thumbnail。
export interface PhotoRef {
  file_id: string
  is_primary: boolean
  sort: number
  url: string
}

export interface PhotoLinkInput {
  file_ids: string[]
  primary_file_id?: string
}

export interface AiMaskedSettings {
  provider: string
  base_url: string
  model: string
  timeout_seconds: number
  max_images: number
  enabled: boolean
  api_key_set: boolean
  api_key_hint?: string
  rekey_needed?: boolean
  updated_at?: string
}

export interface AiTestResult {
  ok: boolean
  message: string
  detail?: string
  model?: string
  saw_image?: boolean
  candidates?: number
  elapsed_ms?: number
}

// —— AI 识别导入——
// 契约：POST /ai/tasks 202 返回 {task,candidates}；GET /ai/tasks/:id 同形状；
// 确认响应恰好三键 {item_id, public_code, movement_id}（movement_id 可为 null）。
export interface AiTask {
  id: string
  kind: string
  space_id: string
  state: 'pending' | 'running' | 'succeeded' | 'failed' | string
  attempt: number
  created_at: string
  updated_at: string
  input: { mode?: string; image_ids?: string[]; template_id?: string }
  result?: { candidate_count: number; empty?: boolean; provider?: string; model?: string; elapsed_ms?: number; rejected?: number }
  error?: string
}

export interface AiCandidate {
  id: string
  task_id: string
  space_id: string
  index: number
  revision: number
  name: string
  summary?: string
  notes?: string
  fields?: Record<string, unknown>
  applied?: Record<string, string>
  manual_fields?: string[]
  source_image_ids: string[]
  evidence?: string
  confidence?: number
  warnings?: string[]
  state: 'pending' | 'confirmed' | 'discarded' | string
  confirmed_item_id?: string
  confirmed_operation_id?: string
}

export interface AiTaskResult {
  task: AiTask
  candidates: AiCandidate[]
}

export interface AiConfirmResult {
  item_id: string
  public_code: string
  movement_id: string | null
}
