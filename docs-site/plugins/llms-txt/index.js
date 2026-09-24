/**
 * llms.txt plugin for Docusaurus.
 *
 * Writes build/llms.txt (the llmstxt.org format: an H1, a one-paragraph
 * summary, then sections of `- [Title](url): one sentence`) and
 * build/llms-full.txt (the concatenated Markdown of the "Start here" pages), so
 * an agent can be pointed at one URL instead of answering from stale memory.
 *
 * Built from the hand-authored Markdown in docs/guides and docs/usage, not from
 * the rendered HTML: titles come from frontmatter, the sentence from the first
 * paragraph after the H1.
 *
 * Every URL uses `publicUrl`, never siteConfig.url. The same build also ships
 * to a LAN-only Gitea Pages host (DOCS_URL), and a file whose job is to be
 * handed to agents must only ever name the public site. The build FAILS if the
 * output would be empty, is missing a required page, or contains a private
 * host.
 */

const fs = require('fs');
const path = require('path');

const PRIVATE_HOST = /(gitea\.stump\.rocks|\.pages\.stump\.rocks|stump\.rocks)/i;

// splitFrontmatter returns [frontmatter fields, body] for a Markdown file. Only
// flat `key: value` lines are read: that is all the guides use.
function splitFrontmatter(src) {
  const m = src.match(/^---\n([\s\S]*?)\n---\n?/);
  if (!m) return [{}, src];
  const fields = {};
  for (const line of m[1].split('\n')) {
    const kv = line.match(/^([A-Za-z_]+):\s*(.*)$/);
    if (kv) fields[kv[1]] = kv[2].replace(/^["']|["']$/g, '');
  }
  return [fields, src.slice(m[0].length)];
}

// plainText strips the Markdown a one-line description should not carry.
function plainText(s) {
  return s
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1') // links → their text
    .replace(/[*_`]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

// firstSentence finds the first prose paragraph after the H1 (skipping
// admonitions, tables, lists, code and headings) and returns its first sentence.
function firstSentence(body) {
  const paras = body.split(/\n\s*\n/);
  let inFence = false;
  for (const raw of paras) {
    const p = raw.trim();
    const fences = (p.match(/^```/gm) || []).length;
    if (inFence) { if (fences % 2 === 1) inFence = false; continue; }
    if (fences % 2 === 1) { inFence = true; continue; }
    if (!p || /^(#|:::|\||[-*] |\d+\. |```|<|>)/.test(p)) continue;
    const text = plainText(p);
    const m = text.match(/^(.+?[.!?])(\s|$)/);
    return m ? m[1] : text;
  }
  return '';
}

// readPages reads every .md file in dir into {slug, route, title, description, body}.
function readPages(docsRoot, dir) {
  const abs = path.join(docsRoot, dir);
  if (!fs.existsSync(abs)) return [];
  return fs.readdirSync(abs)
    .filter((f) => f.endsWith('.md'))
    .map((f) => {
      const src = fs.readFileSync(path.join(abs, f), 'utf8');
      const [fm, body] = splitFrontmatter(src);
      const slug = f.replace(/\.md$/, '');
      return {
        slug,
        route: slug === 'index' ? `${dir}/` : `${dir}/${slug}`,
        position: Number(fm.sidebar_position ?? 99),
        title: fm.title || slug,
        description: firstSentence(body),
        body,
      };
    })
    .sort((a, b) => a.position - b.position || a.slug.localeCompare(b.slug));
}

function entry(publicUrl, page) {
  const desc = page.description ? `: ${page.description}` : '';
  return `- [${page.title}](${publicUrl}${page.route})${desc}`;
}

/**
 * render builds both files' contents. Exported for the unit test.
 *
 * @param {object} o
 * @param {string} o.docsRoot   repo docs/ directory
 * @param {string} o.publicUrl  absolute public site URL, ending in "/"
 * @param {string[]} o.startHere routes to list first ("guides/onboarding")
 * @param {string} o.title
 * @param {string} o.summary
 */
function render({ docsRoot, publicUrl, startHere, title, summary }) {
  const guides = readPages(docsRoot, 'guides');
  const usage = readPages(docsRoot, 'usage');
  const all = [...guides, ...usage];
  const byRoute = new Map(all.map((p) => [p.route, p]));

  const missing = startHere.filter((r) => !byRoute.has(r));
  if (missing.length) {
    throw new Error(`llms-txt: required page(s) not found: ${missing.join(', ')}`);
  }
  const first = startHere.map((r) => byRoute.get(r));
  const rest = (list) => list.filter((p) => !startHere.includes(p.route));

  const lines = [
    `# ${title}`,
    '',
    `> ${summary}`,
    '',
    '## Start here',
    '',
    ...first.map((p) => entry(publicUrl, p)),
    '',
    '## Guides',
    '',
    ...rest(guides).map((p) => entry(publicUrl, p)),
    '',
    '## Reference',
    '',
    ...rest(usage).map((p) => entry(publicUrl, p)),
    '',
    '## Optional',
    '',
    `- [Architecture decisions](${publicUrl}decisions): every ADR, with status and the decision it records.`,
    `- [Specifications](${publicUrl}specs): the requirements each feature is built and tested against.`,
    '',
  ];
  const llms = lines.join('\n');

  const full = [
    `# ${title}: full text of the "Start here" pages`,
    '',
    `> ${summary}`,
    '',
    ...first.map((p) => [`<!-- ${publicUrl}${p.route} -->`, `# ${p.title}`, '', p.body.replace(/^#\s.*\n/, '').trim(), ''].join('\n')),
  ].join('\n');

  return { llms, full, count: all.length };
}

// check enforces the build contract on rendered output: not empty (for an
// index, at least one `- [Title](url)` entry), no private host, and every
// required URL present.
function check(name, text, { requiredUrls = [], isIndex = true } = {}) {
  if (!text || !text.trim() || (isIndex && !text.includes('\n- ['))) {
    throw new Error(`llms-txt: ${name} would be empty`);
  }
  const priv = text.match(PRIVATE_HOST);
  if (priv) {
    throw new Error(`llms-txt: ${name} contains a private host (${priv[0]})`);
  }
  for (const u of requiredUrls) {
    if (!text.includes(u)) throw new Error(`llms-txt: ${name} is missing ${u}`);
  }
}

module.exports = function llmsTxtPlugin(context, options) {
  const opts = {
    docsDir: '../docs',
    title: context.siteConfig.title,
    summary: context.siteConfig.tagline,
    ...options,
  };
  if (!/^https:\/\/[^/]+\/.*\/$/.test(opts.publicUrl || '')) {
    throw new Error('llms-txt: publicUrl must be an absolute https URL ending in "/"');
  }
  return {
    name: 'llms-txt',
    async postBuild({ outDir }) {
      const docsRoot = path.resolve(context.siteDir, opts.docsDir);
      const { llms, full, count } = render({ ...opts, docsRoot });
      const required = opts.startHere.map((r) => opts.publicUrl + r);
      check('llms.txt', llms, { requiredUrls: required });
      check('llms-full.txt', full, { isIndex: false });
      fs.writeFileSync(path.join(outDir, 'llms.txt'), llms);
      fs.writeFileSync(path.join(outDir, 'llms-full.txt'), full);
      console.log(`[llms-txt] wrote llms.txt (${count} pages) and llms-full.txt (${opts.startHere.length} pages)`);
    },
  };
};

module.exports.render = render;
module.exports.check = check;
module.exports.firstSentence = firstSentence;
