import { Route, Routes } from 'react-router-dom';
import { Layout } from './components/Layout';
import { Launchpad } from './pages/Launchpad';
import { SetupWizard } from './pages/SetupWizard';
import { LibraryPage } from './pages/LibraryPage';
import { ImportPage } from './pages/ImportPage';
import { JobsPage } from './pages/JobsPage';
import { UsersPage } from './pages/UsersPage';
import { SettingsPage } from './pages/SettingsPage';
import { NotFound } from './pages/NotFound';

export function App() {
  return (
    <Routes>
      {/* Setup is full-bleed (no side nav) — it's the first-run flow. */}
      <Route path="/setup" element={<SetupWizard />} />

      {/* Everything else lives inside the management chrome. */}
      <Route
        path="/"
        element={
          <Layout>
            <Launchpad />
          </Layout>
        }
      />
      <Route
        path="/library"
        element={
          <Layout>
            <LibraryPage />
          </Layout>
        }
      />
      <Route
        path="/import"
        element={
          <Layout>
            <ImportPage />
          </Layout>
        }
      />
      <Route
        path="/jobs"
        element={
          <Layout>
            <JobsPage />
          </Layout>
        }
      />
      <Route
        path="/manage/users"
        element={
          <Layout>
            <UsersPage />
          </Layout>
        }
      />
      <Route
        path="/settings"
        element={
          <Layout>
            <SettingsPage />
          </Layout>
        }
      />
      <Route
        path="*"
        element={
          <Layout>
            <NotFound />
          </Layout>
        }
      />
    </Routes>
  );
}
