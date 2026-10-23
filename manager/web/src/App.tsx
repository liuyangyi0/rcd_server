import { lazy, Suspense } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';
import { AppShell } from './components/AppShell';
import { LoadingState } from './components/PageState';
import { AuthGate } from './components/AuthGate';

const OverviewPage = lazy(() => import('./pages/OverviewPage'));
const GroupsPage = lazy(() => import('./pages/GroupsPage'));
const SourcesPage = lazy(() => import('./pages/SourcesPage'));
const PointsPage = lazy(() => import('./pages/PointsPage'));
const DeviceVersionsPage = lazy(() => import('./pages/DeviceVersionsPage'));
const CalculationsPage = lazy(() => import('./pages/CalculationsPage'));
const CommandsPage = lazy(() => import('./pages/CommandsPage'));
const LowerLogsPage = lazy(() => import('./pages/LowerLogsPage'));
const OperationsPage = lazy(() => import('./pages/OperationsPage'));
const SettingsPage = lazy(() => import('./pages/SettingsPage'));
const HelpPage = lazy(() => import('./pages/HelpPage'));

export default function App() {
  return (
    <AuthGate><Routes>
      <Route element={<AppShell />}>
        <Route index element={<Suspense fallback={<LoadingState />}><OverviewPage /></Suspense>} />
        <Route path="groups" element={<Suspense fallback={<LoadingState />}><GroupsPage /></Suspense>} />
        <Route path="sources" element={<Suspense fallback={<LoadingState />}><SourcesPage /></Suspense>} />
        <Route path="points" element={<Suspense fallback={<LoadingState />}><PointsPage /></Suspense>} />
        <Route path="device-versions" element={<Suspense fallback={<LoadingState />}><DeviceVersionsPage /></Suspense>} />
		<Route path="calculations" element={<Suspense fallback={<LoadingState />}><CalculationsPage /></Suspense>} />
        <Route path="commands" element={<Suspense fallback={<LoadingState />}><CommandsPage /></Suspense>} />
        <Route path="lower-logs" element={<Suspense fallback={<LoadingState />}><LowerLogsPage /></Suspense>} />
        <Route path="operations" element={<Suspense fallback={<LoadingState />}><OperationsPage /></Suspense>} />
        <Route path="settings" element={<Suspense fallback={<LoadingState />}><SettingsPage /></Suspense>} />
        <Route path="help" element={<Suspense fallback={<LoadingState />}><HelpPage /></Suspense>} />
        <Route path="*" element={<Navigate replace to="/" />} />
      </Route>
    </Routes></AuthGate>
  );
}
