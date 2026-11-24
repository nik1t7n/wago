// Builds the site's pages from the repository's own Markdown, so the docs have one source: docs/*.md, AGENTS.md,
// README sections, CHANGELOG.md, SECURITY.md and examples/README.md. Links between those files become site links,
// links to other repository files go to GitHub, and diagrams are copied next to the pages.
import { readFileSync, writeFileSync, mkdirSync, readdirSync, rmSync, copyFileSync, existsSync, statSync } from 'node:fs';
import { dirname, join, normalize, posix } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const out = join(root, 'site', 'src', 'content', 'docs');
const assets = join(root, 'site', 'public', 'assets');
const base = '/wago';
const repo = 'https://github.com/nik1t7n/wago';

// page slug -> where its text comes from
const pages = [
  { slug: 'getting-started', title: 'Getting started', from: 'README.md',
    sections: ['Quickstart', 'One turn, start to finish', 'Plug in any agent', 'Make it yours'] },
  { slug: 'for-agents', title: 'For AI agents', from: 'AGENTS.md' },
  { slug: 'examples', title: 'Examples', from: 'examples/README.md' },
  { slug: 'agent-layer', title: 'The agent layer', from: 'docs/AGENT-LAYER.md' },
  { slug: 'configuration', title: 'Configuration', from: 'docs/CONFIG.md' },
  { slug: 'migration', title: 'Moving from WAHA', from: 'docs/MIGRATION.md' },
  { slug: 'security', title: 'Safety and security', from: 'SECURITY.md', prepend: { from: 'README.md', sections: ['Play it safe'] } },
  { slug: 'api', title: 'API reference', from: 'docs/API.md' },
  { slug: 'design', title: 'Design notes', from: 'docs/DESIGN.md' },
  { slug: 'changelog', title: 'Changelog', from: 'CHANGELOG.md' },
];
const slugOf = new Map(pages.filter((p) => !p.sections).map((p) => [p.from, p.slug]));
slugOf.set('README.md', '');

const read = (f) => readFileSync(join(root, f), 'utf8');

// the "## Heading" sections of a file, each up to the next heading of the same level
function sections(text, names) {
  const parts = text.split(/^(?=## )/m);
  return names.map((n) => {
    const s = parts.find((p) => p.startsWith(`## ${n}\n`));
    if (!s) throw new Error(`README has no section "${n}"`);
    return s.trimEnd();
  }).join('\n\n');
}

function dropTitle(text) {
  return text.replace(/^# .*\n+/, '');
}

// a link target written in `file` -> a site link, an asset or a GitHub link
function rewrite(target, file) {
  if (/^([a-z]+:|#|\/)/i.test(target)) return target;
  const [path, hash = ''] = target.split('#');
  const anchor = hash ? `#${hash}` : '';
  const abs = posix.normalize(posix.join(posix.dirname(file), path)).replace(/^\.\//, '');
  if (slugOf.has(abs)) return `${base}/${slugOf.get(abs)}${slugOf.get(abs) ? '/' : ''}${anchor}`;
  if (abs.startsWith('docs/assets/')) return `${base}/assets/${posix.basename(abs)}`;
  const isDir = existsSync(join(root, abs)) && statSync(join(root, abs)).isDirectory();
  return `${repo}/${isDir ? 'tree' : 'blob'}/main/${abs}${anchor}`;
}

function links(text, file) {
  return text
    .replace(/(\]\()([^)\s]+)(\))/g, (_, a, t, b) => a + rewrite(t, file) + b)
    .replace(/((?:src|href)=")([^"]+)(")/g, (_, a, t, b) => a + rewrite(t, file) + b);
}

const yaml = (s) => JSON.stringify(s);

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
mkdirSync(assets, { recursive: true });
for (const f of readdirSync(join(root, 'docs', 'assets'))) copyFileSync(join(root, 'docs', 'assets', f), join(assets, f));
// the landing page is written by hand and kept in git
copyFileSync(join(root, 'site', 'src', 'landing.mdx'), join(out, 'index.mdx'));

for (const p of pages) {
  let body = p.sections ? sections(read(p.from), p.sections) : dropTitle(read(p.from));
  body = links(body, p.from);
  if (p.prepend) body = links(sections(read(p.prepend.from), p.prepend.sections), p.prepend.from) + '\n\n' + body;
  const front = `---\ntitle: ${yaml(p.title)}\neditUrl: ${yaml(`${repo}/edit/main/${p.from}`)}\n---\n\n`;
  writeFileSync(join(out, `${p.slug}.md`), front + body.trim() + '\n');
}
console.log(`synced ${pages.length} pages and ${readdirSync(assets).length} assets`);
