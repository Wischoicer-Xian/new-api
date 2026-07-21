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

/**
 * Detect the nginx proxy base path at runtime.
 *
 * Supports multiple proxy configurations:
 * - /token-platform-fe  (workstation reverse proxy)
 * - /token-platform     (direct nginx proxy)
 * - empty string        (direct access to the Go server)
 *
 * Detection is purely client-side: it reads `window.location.pathname`
 * once at module load time and returns the longest matching prefix.
 */
export function getBasepath(): string {
  if (typeof window === 'undefined') return ''
  const pathname = window.location.pathname
  // Order matters: check longest prefix first
  if (pathname.startsWith('/token-platform-fe')) return '/token-platform-fe'
  if (pathname.startsWith('/token-platform')) return '/token-platform'
  return ''
}

/**
 * Prepend the detected basepath to a relative path.
 *
 * Use this for `window.location.href` / `window.location.replace()` calls
 * that bypass TanStack Router (which handles basepath automatically via
 * `<Link>` and `navigate()`). Without this helper, hard-coded relative
 * paths like `/sign-in` would lose the proxy prefix.
 *
 * @example
 * window.location.href = withBasepath('/sign-in')
 * // → '/token-platform-fe/sign-in' when behind the workstation proxy
 */
export function withBasepath(path: string): string {
  const bp = getBasepath()
  if (!bp) return path
  if (path.startsWith(bp)) return path
  if (path.startsWith('/')) return `${bp}${path}`
  return `${bp}/${path}`
}
