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
 * Runtime public path for rspack/webpack async chunk loading.
 *
 * MUST be the first import in main.tsx (before any dynamic imports).
 *
 * With `assetPrefix: './'`, rspack generates relative chunk URLs that resolve
 * against `window.location`. After SPA navigation to `/dashboard/overview`,
 * `./static/js/async/chunk.js` becomes `/dashboard/static/js/async/chunk.js`
 * which 404s. This module sets `__webpack_public_path__` to an absolute path
 * based on the detected basepath, fixing chunk loading for all entry paths:
 *
 * rspack's chunk URL function already produces paths starting with
 * `static/js/async/`, so __webpack_public_path__ must be just the basepath
 * with a trailing slash — NOT `base + '/static/js/'` (that doubles the path).
 *
 * - Direct Go server (:18083) → `/`
 * - /token-platform nginx proxy → `/token-platform/`
 * - /token-platform-fe workstation proxy → `/token-platform-fe/`
 */

declare const __webpack_public_path__: string

if (typeof window !== 'undefined') {
  const pathname = window.location.pathname
  let base = ''
  if (pathname.startsWith('/token-platform-fe')) base = '/token-platform-fe'
  else if (pathname.startsWith('/token-platform')) base = '/token-platform'
  // rspack chunk URLs already include 'static/js/async/' — only set the basepath root
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  ;(__webpack_public_path__ as any) = base + '/'
}
