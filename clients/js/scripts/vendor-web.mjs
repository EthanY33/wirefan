// Copies the built ESM bundle into web/ so the demo page (embedded into the
// Go binary via web/embed.go) can import it without a bundler or CDN.
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, "..", "dist", "index.js");
const dest = join(here, "..", "..", "..", "web", "wirefan-client.js");

const header = `// VENDORED BUILD, do not edit by hand.
// Source: clients/js/src/index.ts. Regenerate with:
//   cd clients/js && npm run vendor:web
`;

writeFileSync(dest, header + readFileSync(src, "utf8"));
console.log(`vendored ${src} -> ${dest}`);
