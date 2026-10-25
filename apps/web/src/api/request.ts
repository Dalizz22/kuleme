// 统一请求封装：解 {data,request_id} / {error:{code,message,details}} 外壳。
// 规则：HTTP 状态码本身反映失败；网络失败（TypeError）对幂等请求最多重试 3 次退避；
// 所有错误以 ApiError 抛出，调用方负责提示并在 finally 里复位 loading——
// 封装层绝不吞错误，也就不存在 toast 完卡转圈。
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: Record<string, unknown> = {},
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  body?: unknown
  /** FormData 时不设 Content-Type，让浏览器带 boundary。 */
  form?: FormData
  idempotencyKey?: string
  /** 网络层失败重试次数（仅瞬时失败，4xx/5xx 业务错误不重试）。 */
  retries?: number
}

interface Envelope<T> {
  data?: T
  error?: { code: string; message: string; details?: Record<string, unknown> }
  request_id?: string
}

const BASE = '/api/v1'

/** 401 统一出口：由路由层注册，跳登录页。/sessions 与 /me 自身除外。 */
let unauthorizedHandler: (() => void) | null = null
export function onUnauthorized(fn: () => void): void {
  unauthorizedHandler = fn
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

export async function request<T = unknown>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, form, idempotencyKey, retries = 2 } = opts
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey

  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      const res = await fetch(BASE + path, {
        method,
        headers,
        credentials: 'same-origin',
        body: form ?? (body !== undefined ? JSON.stringify(body) : undefined),
      })
      const payload = (await res.json().catch(() => ({}))) as Envelope<T>
      if (!res.ok) {
        const e = payload.error
        const apiErr = new ApiError(res.status, e?.code ?? 'INTERNAL', e?.message ?? `请求失败（${res.status}）`, e?.details)
        if (res.status === 401 && path !== '/sessions' && path !== '/me') unauthorizedHandler?.()
        throw apiErr
      }
      return payload.data as T
    } catch (err) {
      if (err instanceof ApiError) throw err
      // fetch TypeError = 网络失败/连接中断，可退避重试
      if (attempt < retries) await sleep(300 * 2 ** attempt)
    }
  }
  throw new ApiError(0, 'NETWORK', '网络连接失败，请检查服务是否在线')
}

/** 幂等键：同一业务动作失败重试必须复用同一键。 */
export function newIdempotencyKey(): string {
  return crypto.randomUUID()
}

/** 分页响应外壳（data 内层）。 */
export interface Page<T> {
  items: T[]
  page: number
  page_size: number
  total: number
}
