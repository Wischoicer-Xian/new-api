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

/** Wischoicer 自动注册到 new-api 时使用的托管用户属组。 */
export const WISCHOICER_MANAGED_USER_GROUP = '知言云策'

/**
 * 托管用户统一从 Wischoicer“积分与成本”充值，new-api 不再提供第二入口。
 * 属组由 new-api 登录态的 user.group 提供；未知或其他属组保持原有钱包能力。
 */
export function canAccessNewApiWallet(group: unknown): boolean {
  return group !== WISCHOICER_MANAGED_USER_GROUP
}
