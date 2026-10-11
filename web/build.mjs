// Builds the frontend into ../internal/web/dist so it gets embedded in the
// Go binary.
import { build } from 'esbuild';
import { cpSync, mkdirSync } from 'node:fs';

const outDir = '../internal/web/dist';
mkdirSync(outDir, { recursive: true });

await build({
  entryPoints: ['src/main.ts'],
  bundle: true,
  minify: true,
  outfile: `${outDir}/main.js`,
});

cpSync('index.html', `${outDir}/index.html`);
cpSync('styles.css', `${outDir}/styles.css`);
console.log('frontend built to', outDir);
