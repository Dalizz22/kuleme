import { createRouter, createWebHistory } from 'vue-router'
import { onUnauthorized } from '../api/request'
import { useAuthStore } from '../stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('../views/LoginView.vue') },
    { path: '/setup', name: 'setup', component: () => import('../views/SetupView.vue') },
    {
      path: '/',
      component: () => import('../layout/AdminLayout.vue'),
      children: [
        { path: '', name: 'dashboard', component: () => import('../views/DashboardView.vue') },
        { path: 'items', name: 'items', component: () => import('../views/items/ItemListView.vue') },
        { path: 'items/new', name: 'item-new', component: () => import('../views/items/ItemCreateView.vue') },
        { path: 'items/:id', name: 'item-detail', component: () => import('../views/items/ItemDetailView.vue') },
        { path: 'bases', name: 'bases', component: () => import('../views/BaseDataView.vue') },
        { path: 'io', name: 'io', component: () => import('../views/ImportExportView.vue') },
        { path: 'ai/import', name: 'ai-import', component: () => import('../views/ai/AiImportView.vue') },
        { path: 'recycle', name: 'recycle', component: () => import('../views/RecycleView.vue') },
        { path: 'messages', name: 'messages', component: () => import('../views/MessagesView.vue') },
        { path: 'settings/ai', name: 'settings-ai', component: () => import('../views/settings/AiSettingsView.vue') },
      ],
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.booted) {
    try {
      await auth.boot()
    } catch {
      // system/info 都拿不到：服务未就绪，放行到登录页由页面报错
      auth.booted = true
    }
  }
  if (!auth.initialized) return to.name === 'setup' ? true : { name: 'setup' }
  if (to.name === 'setup') return { name: 'dashboard' }
  if (to.name === 'login') return auth.loggedIn ? { name: 'dashboard' } : true
  if (!auth.loggedIn) return { name: 'login', query: { redirect: to.fullPath } }
  return true
})

onUnauthorized(() => {
  const auth = useAuthStore()
  auth.onSessionExpired()
  if (router.currentRoute.value.name !== 'login') {
    void router.push({ name: 'login', query: { redirect: router.currentRoute.value.fullPath } })
  }
})

export default router
