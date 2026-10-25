import { ApiError, request } from './request'

// 文案唯一真源在本文件；Android 客户端的 src/api.ts 的 errorText/ERROR_TEXT 必须逐字一致。
// 服务端错误码清单来自 server/internal/httpx/response.go + 各包 Code* 常量，穷尽式对齐（差分脚本见交付）。
// 原则：服务端中文 business message 可展示的优先显示；纯格式/鉴权/冲突类用固定文案；
// 只有 NOT_IMPLEMENTED 才允许出现「功能未开放」；400 已彻底消失，不要按 status===400 分支。

/** 服务端全部错误码 → 中文固定文案（默认兜底文案）。 */
export const ERROR_TEXT: Record<string, string> = {
  // httpx 通用码
  VALIDATION_FAILED: '输入有误，请检查后重试',
  INVALID_CODE: '数据校验未通过，请刷新后重试',
  UNAUTHENTICATED: '登录已失效，请重新登录',
  SESSION_EXPIRED: '登录已失效，请重新登录',
  FORBIDDEN: '没有权限执行该操作',
  NOT_FOUND: '对象不存在或无权访问',
  VERSION_CONFLICT: '数据已变化，请刷新后重试',
  IDEMPOTENCY_CONFLICT: '本次操作已用不同内容提交过，请勿重复提交',
  INSUFFICIENT_STOCK: '库存不足，请核对数量',
  TREE_CYCLE: '不能移动到自己的子节点下',
  NODE_IN_USE: '该节点仍被使用，无法删除',
  OPEN_BUSINESS: '当前状态不允许该操作，请刷新后重试',
  FILE_TOO_LARGE: '文件超出大小限制',
  FILE_IN_USE: '文件已被引用，无法删除',
  RATE_LIMITED: '请求过于频繁，请稍后重试',
  SERVICE_UNAVAILABLE: '服务暂不可用，请稍后重试',
  MAINTENANCE: '系统维护中，请稍后重试',
  INTERNAL: '服务处理失败，请稍后重试',
  INVALID_IDEMPOTENCY_KEY: '请求标识不合法，本次操作未提交，请重新发起',
  INVALID_IMAGE_COUNT: '图片数量超出限制，请减少后重试',
  PAYLOAD_TOO_LARGE: '提交内容过大，请精简后重试',
  // 各包自定义码
  FEATURE_DISABLED: '该功能已被管理员关闭',
  INVALID_LOCATION: '位置不存在或不可用，请重新选择',
  INVALID_STATUS: '状态筛选值无效，请重新选择',
  INVALID_TRANSFER_TARGET: '不能转库到当前库，库内移位请用库存移动',
  PHOTO_ALREADY_LINKED: '照片已关联到其他物品，请勿重复关联',
  // 客户端契约预留（服务端尚未使用，客户端兜底）
  NOT_IMPLEMENTED: '功能未开放',
}

/**
 * 这些码优先用固定文案：鉴权/冲突/格式类，服务端 message 可能是技术细节。
 * 其余码若服务端给了中文 message（业务提示），优先展示 message。
 */
const FIXED_TEXT_CODES = new Set([
  'UNAUTHENTICATED',
  'SESSION_EXPIRED',
  'FORBIDDEN',
  'NOT_FOUND',
  'VERSION_CONFLICT',
  'IDEMPOTENCY_CONFLICT',
  'INVALID_IDEMPOTENCY_KEY',
  'PAYLOAD_TOO_LARGE',
  'FILE_TOO_LARGE',
  'RATE_LIMITED',
  'INTERNAL',
  'NOT_IMPLEMENTED',
])

/** 只看是否含中文，避免把英文技术栈信息直接丢给用户。 */
function hasChinese(s: string) {
  return /[\u4e00-\u9fff]/.test(s)
}

export function errorText(e: unknown, fallback = '请求失败'): string {
  if (e instanceof ApiError) {
    const fixed = ERROR_TEXT[e.code]
    if (e.code === 'NOT_IMPLEMENTED') return fixed
    if (e.code && FIXED_TEXT_CODES.has(e.code) && fixed) return fixed
    const msg = (e.message || '').trim()
    if (msg && hasChinese(msg)) return msg
    if (fixed) return fixed
    // 服务端没给可展示 message：按状态码给类别文案（不暴露技术细节）。
    switch (e.status) {
      case 401:
        return ERROR_TEXT.UNAUTHENTICATED
      case 403:
        return ERROR_TEXT.FORBIDDEN
      case 404:
        return ERROR_TEXT.NOT_FOUND
      case 409:
        return ERROR_TEXT.VERSION_CONFLICT
      case 413:
        return ERROR_TEXT.FILE_TOO_LARGE
      case 422:
        return ERROR_TEXT.VALIDATION_FAILED
      case 429:
        return ERROR_TEXT.RATE_LIMITED
      case 503:
        return ERROR_TEXT.SERVICE_UNAVAILABLE
      default:
        return msg || fallback
    }
  }
  // fetch 网络层失败是 TypeError，不是服务端错误码。
  if (e instanceof TypeError) return '网络连接失败，请检查服务器地址后重试'
  if (e instanceof Error) return e.message || fallback
  return fallback
}

/** 服务端错误码（无则空串），用于在提示里附带 code，便于对照契约。 */
export function errorCode(e: unknown): string {
  return e instanceof ApiError ? e.code : ''
}

export function isNotImplemented(e: unknown): boolean {
  return e instanceof ApiError && e.code === 'NOT_IMPLEMENTED'
}

/**
 * 写操作失败但可能已提交：网络断连、5xx、409。
 * 此时不能换新幂等键重提，先按同一个 UUID（既当 Idempotency-Key 又当 operation_id）查终态。
 */
export function isUncertainWriteFailure(e: unknown): boolean {
  if (e instanceof TypeError) return true
  if (e instanceof ApiError) return e.status === 0 || e.status >= 500 || e.status === 409
  return false
}

/** GET /operation-results/:operation_id 的 data（契约 idem.OperationResult）。 */
export interface OperationResult<T = unknown> {
  operation_id: string
  status: 'in_progress' | 'succeeded' | 'failed'
  result: T | null
  error_code: string | null
  created_at: string
  updated_at: string
}

export function getOperationResult<T = unknown>(operationId: string) {
  return request<OperationResult<T>>(`/operation-results/${operationId}`)
}

export interface RecoveryOutcome<T = unknown> {
  /** true = 服务端已确认生效，调用方应刷新页面按成功处理。 */
  recovered: boolean
  message: string
  result?: T | null
}

/**
 * 响应丢失恢复：用失败那次请求的幂等键查结果，而不是重复提交。
 * 404 = 服务端没登记本次操作（可能根本没到服务端），提示可安全重试。
 */
export async function recoverFailedWrite<T = unknown>(
  operationId: string,
  failure: unknown,
  fallback = '请求失败',
): Promise<RecoveryOutcome<T>> {
  if (!isUncertainWriteFailure(failure)) {
    return { recovered: false, message: errorText(failure, fallback) }
  }
  try {
    const r = await getOperationResult<T>(operationId)
    if (r.status === 'succeeded') {
      return { recovered: true, message: '服务端已确认本次操作生效', result: r.result }
    }
    if (r.status === 'in_progress') {
      return { recovered: false, message: '服务端仍在处理本次操作，请稍后刷新查看结果（勿重复提交）' }
    }
    return {
      recovered: false,
      message: `${errorText(failure, fallback)}；服务端已登记本次操作未生效，可重试`,
    }
  } catch (q) {
    if (q instanceof ApiError && q.status === 404) {
      return { recovered: false, message: '服务端未登记本次操作，可重试' }
    }
    return { recovered: false, message: errorText(failure, fallback) }
  }
}
