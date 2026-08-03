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
import { describe, expect, it } from 'vitest'

import {
  canAccessNewApiWallet,
  WISCHOICER_MANAGED_USER_GROUP,
} from '../new-api-wallet-access'

describe('new-api wallet access by user group', () => {
  it('denies the wallet to Wischoicer-managed users', () => {
    expect(canAccessNewApiWallet(WISCHOICER_MANAGED_USER_GROUP)).toBe(false)
  })

  it.each([undefined, null, '', 'default', 'wis285-test-group'])(
    'keeps the wallet available for non-managed group %s',
    (group) => {
      expect(canAccessNewApiWallet(group)).toBe(true)
    }
  )

  it('uses an exact group match so unrelated names are not hidden', () => {
    expect(canAccessNewApiWallet('知言云策测试组')).toBe(true)
  })
})
