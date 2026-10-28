import { createRoot } from "react-dom/client";
import { LisaApp } from "./LisaApp";

const root = document.getElementById("root");
if (!root) throw new Error("missing #root");
createRoot(root).render(<LisaApp />);
