import { Navigate, Route, Routes } from 'react-router-dom';
import LoginPage from './pages/Login';
import RegisterPage from './pages/Register';
import ForgotPasswordPage from './pages/ForgotPassword';
import UserProfilePage from './pages/UserProfile';

export function AppRouter() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/login" replace />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/register" element={<RegisterPage />} />
      <Route path="/forgot-password" element={<ForgotPasswordPage />} />
      {/* :id 是动态段,匹配 /users/1、/users/42……页面里用 useParams 取出来 */}
      <Route path="/users/:id" element={<UserProfilePage />} />
      <Route path="*" element={<Navigate to="/login" replace />} />
    </Routes>
  );
}