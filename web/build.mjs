import { copyFile, mkdir } from 'node:fs/promises';

await mkdir('static/icons', { recursive: true });
await mkdir('static/licenses', { recursive: true });
await copyFile('node_modules/htmx.org/dist/htmx.min.js', 'static/htmx.min.js');
await copyFile('src/app.js', 'static/app.js');
for (const icon of ['pause', 'play']) {
  await copyFile(`node_modules/lucide-static/icons/${icon}.svg`, `static/icons/${icon}.svg`);
}
for (const name of ['htmx.org', 'tailwindcss', 'daisyui', 'lucide-static']) {
  await copyFile(`node_modules/${name}/LICENSE`, `static/licenses/${name}.txt`);
}
