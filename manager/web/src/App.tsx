import { lazy, Suspense } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';
import { AppShell } from './components/AppShell';
import { LoadingState } from './components/PageState';

const OverviewPage = lazy(() => import('./pages/OverviewPage'));
const GroupsPage = lazy(() => import('./pages/GroupsPage'));
const SourcesPage = lazy(() => import('./pages/SourcesPage'));
const PointsPage = lazy(() => import('./pages/PointsPage'));
const CommandsPage = lazy(() => import('./pages/CommandsPage'));
const OperationsPage = lazy(() => import('./pages/OperationsPage'));
const SettingsPage = lazy(() => import('./pages/SettingsPage'));

export default function App() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Suspense fallback={<LoadingState />}><OverviewPage /></Suspense>} />
        <Route path="groups" element={<Suspense fallback={<LoadingState />}><GroupsPage /></Suspense>} />
        <Route path="sources" element={<Suspense fallback={<LoadingState />}><SourcesPage /></Suspense>} />
        <Route path="points" element={<Suspense fallback={<LoadingState />}><PointsPage /></Suspense>} />
        <Route path="commands" element={<Suspense fallback={<LoadingState />}><CommandsPage /></Suspense>} />
        <Route path="operations" element={<Suspense fallback={<LoadingState />}><OperationsPage /></Suspense>} />
        <Route path="settings" element={<Suspense fallback={<LoadingState />}><SettingsPage /></Suspense>} />
        <Route path="*" element={<Navigate replace to="/" />} />
      </Route>
    </Routes>
  );
}
