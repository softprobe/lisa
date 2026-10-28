import { useMemo } from "react";
import { LisaApi, type LisaConfig } from "./client";
import { LisaView } from "./LisaView";
import "./app.css";

export type LisaAppProps = { config?: LisaConfig };

export function LisaApp({ config = {} }: LisaAppProps) {
  const api = useMemo(() => new LisaApi(config), [config]);
  return (
    <main className="lisa-app">
      <LisaView
        key={config.chatStorageKey ?? "workspace-unspecified"}
        api={api}
      />
    </main>
  );
}
