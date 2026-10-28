import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  base: "/",
  plugins: [react()],
  server: { proxy: { "/v1": process.env.LISA_API_PROXY || "http://127.0.0.1:8082" } },
  build: { outDir: "dist", emptyOutDir: true },
});
