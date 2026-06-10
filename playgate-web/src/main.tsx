import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider, Navigate } from "react-router-dom";
import { RoomPage } from "./pages/RoomPage";
import { HostPage } from "./pages/HostPage";
import "./styles.css";

const router = createBrowserRouter([
  { path: "/room/:roomId", element: <RoomPage /> },
  { path: "/host", element: <HostPage /> },
  { path: "/", element: <Navigate to="/host" replace /> },
  { path: "*", element: <Navigate to="/host" replace /> },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>,
);
