import { defineStore } from 'pinia'
import { getMe, getSystemInfo, login, logout } from '../api'
import type { SystemInfo, User } from '../api/types'

export const useAuthStore = defineStore('auth', {
  state: () => ({
    user: null as User | null,
    system: null as SystemInfo | null,
    /** booted: 首次 system/info + /me 探测已完成，路由守卫放行 */
    booted: false,
  }),
  getters: {
    initialized: (s) => s.system?.initialized ?? false,
    loggedIn: (s) => s.user !== null,
  },
  actions: {
    /** 启动探测：匿名可读 system/info；/me 401 视为未登录。 */
    async boot() {
      this.system = await getSystemInfo()
      if (this.system.initialized) {
        try {
          this.user = await getMe()
        } catch {
          this.user = null
        }
      }
      this.booted = true
    },
    async login(username: string, password: string) {
      const res = await login(username, password)
      this.user = res.user
    },
    async logout() {
      try {
        await logout()
      } finally {
        this.user = null
      }
    },
    onSessionExpired() {
      this.user = null
    },
  },
})
