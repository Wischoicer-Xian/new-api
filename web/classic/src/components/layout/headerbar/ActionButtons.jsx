/*
Copyright (C) 2025 QuantumNous

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

import React from 'react';
import { LayoutDashboard } from 'lucide-react';
import { Button, Tooltip } from '@douyinfe/semi-ui';
import NewYearButton from './NewYearButton';
import NotificationButton from './NotificationButton';
import ThemeToggle from './ThemeToggle';
import LanguageSelector from './LanguageSelector';
import UserArea from './UserArea';

/** Get Wischoicer workstation console URL */
function getConsoleUrl() {
  const base = import.meta.env.VITE_WISCHOICER_WORKSTATION_URL || window.location.origin;
  return `${base.replace(/\/+$/, '')}/console`;
}

const ActionButtons = ({
  isNewYear,
  unreadCount,
  onNoticeOpen,
  theme,
  onThemeToggle,
  currentLang,
  onLanguageChange,
  userState,
  isLoading,
  isMobile,
  isSelfUseMode,
  logout,
  navigate,
  t,
}) => {
  return (
    <div className='flex items-center gap-2 md:gap-3'>
      <NewYearButton isNewYear={isNewYear} />

      <Tooltip content='控制台'>
        <Button
          theme='borderless'
          icon={<LayoutDashboard size={18} />}
          onClick={() => window.open(getConsoleUrl(), '_blank', 'noopener,noreferrer')}
        />
      </Tooltip>

      <NotificationButton
        unreadCount={unreadCount}
        onNoticeOpen={onNoticeOpen}
        t={t}
      />

      <ThemeToggle theme={theme} onThemeToggle={onThemeToggle} t={t} />

      <LanguageSelector
        currentLang={currentLang}
        onLanguageChange={onLanguageChange}
        t={t}
      />

      <UserArea
        userState={userState}
        isLoading={isLoading}
        isMobile={isMobile}
        isSelfUseMode={isSelfUseMode}
        logout={logout}
        navigate={navigate}
        t={t}
      />
    </div>
  );
};

export default ActionButtons;
