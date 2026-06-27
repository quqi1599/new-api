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

import React, { lazy, Suspense, useContext, useMemo } from 'react';
import { Route, Routes, useLocation, useParams } from 'react-router-dom';
import Loading from './components/common/ui/Loading';
import { AuthRedirect, PrivateRoute, AdminRoute } from './helpers/auth';
import NotFound from './pages/NotFound';
import Forbidden from './pages/Forbidden';
import { StatusContext } from './context/Status';

import SetupCheck from './components/layout/SetupCheck';

const Home = lazy(() => import('./pages/Home'));
const Dashboard = lazy(() => import('./pages/Dashboard'));
const About = lazy(() => import('./pages/About'));
const UserAgreement = lazy(() => import('./pages/UserAgreement'));
const PrivacyPolicy = lazy(() => import('./pages/PrivacyPolicy'));
const User = lazy(() => import('./pages/User'));
const Setting = lazy(() => import('./pages/Setting'));
const RegisterForm = lazy(() => import('./components/auth/RegisterForm'));
const LoginForm = lazy(() => import('./components/auth/LoginForm'));
const PasswordResetForm = lazy(
  () => import('./components/auth/PasswordResetForm'),
);
const PasswordResetConfirm = lazy(
  () => import('./components/auth/PasswordResetConfirm'),
);
const Channel = lazy(() => import('./pages/Channel'));
const Token = lazy(() => import('./pages/Token'));
const Redemption = lazy(() => import('./pages/Redemption'));
const TopUp = lazy(() => import('./pages/TopUp'));
const Log = lazy(() => import('./pages/Log'));
const Chat = lazy(() => import('./pages/Chat'));
const Chat2Link = lazy(() => import('./pages/Chat2Link'));
const Midjourney = lazy(() => import('./pages/Midjourney'));
const Pricing = lazy(() => import('./pages/Pricing'));
const Task = lazy(() => import('./pages/Task'));
const ModelPage = lazy(() => import('./pages/Model'));
const ModelDeploymentPage = lazy(() => import('./pages/ModelDeployment'));
const Playground = lazy(() => import('./pages/Playground'));
const Subscription = lazy(() => import('./pages/Subscription'));
const OAuth2Callback = lazy(() => import('./components/auth/OAuth2Callback'));
const PersonalSetting = lazy(
  () => import('./components/settings/PersonalSetting'),
);
const Setup = lazy(() => import('./pages/Setup'));

function DynamicOAuth2Callback() {
  const { provider } = useParams();
  return <OAuth2Callback type={provider} />;
}

function App() {
  const location = useLocation();
  const [statusState] = useContext(StatusContext);
  const lazyPage = (children) => (
    <Suspense fallback={<Loading></Loading>} key={location.pathname}>
      {children}
    </Suspense>
  );

  // 获取模型广场权限配置
  const pricingRequireAuth = useMemo(() => {
    const headerNavModulesConfig = statusState?.status?.HeaderNavModules;
    if (headerNavModulesConfig) {
      try {
        const modules = JSON.parse(headerNavModulesConfig);

        // 处理向后兼容性：如果pricing是boolean，默认不需要登录
        if (typeof modules.pricing === 'boolean') {
          return false; // 默认不需要登录鉴权
        }

        // 如果是对象格式，使用requireAuth配置
        return modules.pricing?.requireAuth === true;
      } catch (error) {
        console.error('解析顶栏模块配置失败:', error);
        return false; // 默认不需要登录
      }
    }
    return false; // 默认不需要登录
  }, [statusState?.status?.HeaderNavModules]);

  return (
    <SetupCheck>
      <Routes>
        <Route path='/' element={lazyPage(<Home />)} />
        <Route path='/setup' element={lazyPage(<Setup />)} />
        <Route path='/forbidden' element={<Forbidden />} />
        <Route
          path='/console/models'
          element={<AdminRoute>{lazyPage(<ModelPage />)}</AdminRoute>}
        />
        <Route
          path='/console/deployment'
          element={<AdminRoute>{lazyPage(<ModelDeploymentPage />)}</AdminRoute>}
        />
        <Route
          path='/console/subscription'
          element={<AdminRoute>{lazyPage(<Subscription />)}</AdminRoute>}
        />
        <Route
          path='/console/channel'
          element={<AdminRoute>{lazyPage(<Channel />)}</AdminRoute>}
        />
        <Route
          path='/console/token'
          element={<PrivateRoute>{lazyPage(<Token />)}</PrivateRoute>}
        />
        <Route
          path='/console/playground'
          element={<PrivateRoute>{lazyPage(<Playground />)}</PrivateRoute>}
        />
        <Route
          path='/console/redemption'
          element={<AdminRoute>{lazyPage(<Redemption />)}</AdminRoute>}
        />
        <Route
          path='/console/user'
          element={<AdminRoute>{lazyPage(<User />)}</AdminRoute>}
        />
        <Route
          path='/user/reset'
          element={lazyPage(<PasswordResetConfirm />)}
        />
        <Route
          path='/login'
          element={<AuthRedirect>{lazyPage(<LoginForm />)}</AuthRedirect>}
        />
        <Route
          path='/register'
          element={<AuthRedirect>{lazyPage(<RegisterForm />)}</AuthRedirect>}
        />
        <Route path='/reset' element={lazyPage(<PasswordResetForm />)} />
        <Route
          path='/oauth/github'
          element={lazyPage(<OAuth2Callback type='github'></OAuth2Callback>)}
        />
        <Route
          path='/oauth/discord'
          element={lazyPage(<OAuth2Callback type='discord'></OAuth2Callback>)}
        />
        <Route
          path='/oauth/oidc'
          element={lazyPage(<OAuth2Callback type='oidc'></OAuth2Callback>)}
        />
        <Route
          path='/oauth/linuxdo'
          element={lazyPage(<OAuth2Callback type='linuxdo'></OAuth2Callback>)}
        />
        <Route
          path='/oauth/:provider'
          element={lazyPage(<DynamicOAuth2Callback />)}
        />
        <Route
          path='/console/setting'
          element={<AdminRoute>{lazyPage(<Setting />)}</AdminRoute>}
        />
        <Route
          path='/console/personal'
          element={<PrivateRoute>{lazyPage(<PersonalSetting />)}</PrivateRoute>}
        />
        <Route
          path='/console/topup'
          element={<PrivateRoute>{lazyPage(<TopUp />)}</PrivateRoute>}
        />
        <Route
          path='/console/log'
          element={<PrivateRoute>{lazyPage(<Log />)}</PrivateRoute>}
        />
        <Route
          path='/console'
          element={<PrivateRoute>{lazyPage(<Dashboard />)}</PrivateRoute>}
        />
        <Route
          path='/console/midjourney'
          element={<PrivateRoute>{lazyPage(<Midjourney />)}</PrivateRoute>}
        />
        <Route
          path='/console/task'
          element={<PrivateRoute>{lazyPage(<Task />)}</PrivateRoute>}
        />
        <Route
          path='/pricing'
          element={
            pricingRequireAuth ? (
              <PrivateRoute>{lazyPage(<Pricing />)}</PrivateRoute>
            ) : (
              lazyPage(<Pricing />)
            )
          }
        />
        <Route path='/about' element={lazyPage(<About />)} />
        <Route path='/user-agreement' element={lazyPage(<UserAgreement />)} />
        <Route path='/privacy-policy' element={lazyPage(<PrivacyPolicy />)} />
        <Route path='/console/chat/:id?' element={lazyPage(<Chat />)} />
        {/* 方便使用chat2link直接跳转聊天... */}
        <Route
          path='/chat2link'
          element={<PrivateRoute>{lazyPage(<Chat2Link />)}</PrivateRoute>}
        />
        <Route path='*' element={<NotFound />} />
      </Routes>
    </SetupCheck>
  );
}

export default App;
