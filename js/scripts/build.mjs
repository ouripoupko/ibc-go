// Builds dist/ from src/: the TypeScript compiled to ES modules with their
// type declarations, and the vendored keccak copied as it is.
//
//   node scripts/build.mjs           build dist/
//   node scripts/build.mjs --check   build into a temporary folder, and fail if
//                                    it differs from dist/ (which is committed:
//                                    the node serves it to its own web pages)
import { execFileSync } from 'node:child_process';
import { cpSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const check = process.argv.includes('--check');
const out = check ? mkdtempSync(join(tmpdir(), 'ibc-js-')) : join(root, 'dist');

if (!check) rmSync(out, { recursive: true, force: true });
const tsc = join(root, 'node_modules', 'typescript', 'bin', 'tsc');
execFileSync(process.execPath, [tsc, '-p', root, '--outDir', out], { stdio: 'inherit' });
cpSync(join(root, 'src', 'vendor'), join(out, 'vendor'), { recursive: true });

if (check) {
  const files = (dir) => readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? files(p) : [p];
  });
  const built = files(out).map((p) => relative(out, p)).sort();
  const committed = files(join(root, 'dist')).map((p) => relative(join(root, 'dist'), p)).sort();
  const differ = [...new Set([...built, ...committed])].filter((f) => !built.includes(f) || !committed.includes(f)
    || !readFileSync(join(out, f)).equals(readFileSync(join(root, 'dist', f))));
  rmSync(out, { recursive: true, force: true });
  if (differ.length) {
    console.error(`dist/ is not the build of src/: ${differ.join(', ')}. Run: npm run build`);
    process.exit(1);
  }
  console.log('dist/ is up to date');
}
