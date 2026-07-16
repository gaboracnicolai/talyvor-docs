import { createBrowserRouter, RouterProvider } from "react-router-dom";
import { routes } from "./router/routes";

// App is now just the router mount. Navigation moved from a useState discriminated union to
// URL-driven routing (see src/router/): pages and spaces have real, shareable, refreshable
// addresses, Back/Forward work, and deep links land on the right view. The route tree lives
// in router/routes.tsx so the browser router here and the memory router in tests drive the
// same definition.
const router = createBrowserRouter(routes);

export function App() {
  return <RouterProvider router={router} />;
}
