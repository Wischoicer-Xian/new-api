/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { createFileRoute, redirect } from '@tanstack/react-router'

import { AuthenticatedLayout } from '@/components/layout'
import { saveUserId } from '@/features/auth/lib/storage'
import { getSelf } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

// 内存中的验证标记，避免同一会话中重复验证
let sessionVerified = false

export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async ({ location }) => {
    const { auth } = useAuthStore.getState()

    // If local user exists and session already verified, proceed immediately
    if (auth.user && sessionVerified) {
      return
    }

    // Try to verify session via API.
    // This handles both:
    //   - Existing users who need periodic session re-validation
    //   - SSO login where server session exists but local auth state is empty
    //     (e.g., after POST /api/sso/login set the session cookie)
    if (!sessionVerified) {
      const res = await getSelf().catch(() => null)
      if (res?.success && res.data) {
        auth.setUser(res.data)
        // Persist user ID so the API interceptor can send New-Api-User header.
        // This is needed for both SSO (where localStorage is empty on arrival)
        // and normal login (where it may have been cleared by a page refresh).
        if (res.data.id != null) {
          saveUserId(res.data.id)
        }
        sessionVerified = true
        return
      }
    }

    // No valid session found, redirect to login
    auth.reset()
    throw redirect({
      to: '/sign-in',
      search: { redirect: location.href },
    })
  },
  component: AuthenticatedLayout,
})
