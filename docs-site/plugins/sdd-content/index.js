/**
 * SDD Content Plugin for Docusaurus
 *
 * Consolidated Docusaurus plugin that replaces 8 separate build scripts:
 * - build-spec-mapping.js
 * - graph-data.js (custom YAML parser removed; uses lib-artifact-transforms)
 * - mdx-escape.js
 * - transform-adrs.js
 * - transform-openspecs.js
 * - transform-utils.js
 * - generate-index.js
 * - generate-graph.js
 *
 * This plugin generates MDX files in docs-generated/ from ADR and spec YAML
 * frontmatter, with RFC 2119 highlighting, cross-references, and mini-DAGs.
 * Implements getPathsToWatch() for native Docusaurus hot reload (replaces
 * chokidar-cli + concurrently).
 */

const fs = require('fs');
const path = require('path');
const { parseFrontmatter, extractStatus } = require('lib-artifact-transforms');

const ADR_EDGE_FIELDS = ['supersedes', 'extends', 'enables', 'governs', 'related'];
const SPEC_EDGE_FIELDS = ['implements', 'requires', 'extends', 'supersedes'];
const INVERSE_OF = {
  supersedes: 'superseded-by',
  extends: 'extended-by',
  enables: 'enabled-by',
  governs: 'governed-by',
  implements: 'implemented-by',
  requires: 'depended-on-by',
  related: 'related',
};

// ============================================================================
// MDX Escaping (from mdx-escape.js)
// ============================================================================

function escapeMdxUnsafe(content) {
  const lines = content.split('\n');
  const result = [];
  let inCodeBlock = false;
  let codeFencePattern = null;

  for (const line of lines) {
    const trimmed = line.trimStart();

    const fenceMatch = trimmed.match(/^(`{3,}|~{3,})/);
    if (fenceMatch) {
      const fence = fenceMatch[1];
      if (!inCodeBlock) {
        inCodeBlock = true;
        codeFencePattern = fence[0];
        result.push(line);
        continue;
      } else if (fence[0] === codeFencePattern && trimmed.replace(/^[`~]+/, '').trim() === '') {
        inCodeBlock = false;
        codeFencePattern = null;
        result.push(line);
        continue;
      }
    }

    if (inCodeBlock) {
      result.push(line);
      continue;
    }

    result.push(escapeLineForMdx(line));
  }

  return result.join('\n');
}

function escapeLineForMdx(line) {
  if (isJsxLine(line)) {
    return line;
  }

  let result = '';
  let i = 0;

  while (i < line.length) {
    if (line[i] === '`') {
      const start = i;
      i++;
      while (i < line.length && line[i] !== '`') {
        i++;
      }
      if (i < line.length) i++;
      result += line.slice(start, i);
      continue;
    }

    if (line[i] === '{' && (i === 0 || line[i - 1] !== '\\')) {
      result += '\\{';
      i++;
      continue;
    }
    if (line[i] === '}' && (i === 0 || line[i - 1] !== '\\')) {
      result += '\\}';
      i++;
      continue;
    }

    if (line[i] === '<') {
      const remaining = line.slice(i);

      if (/^<[A-Za-z/!]/.test(remaining)) {
        const tagMatch = remaining.match(/^<\/?([A-Za-z][A-Za-z0-9_-]*)/);
        if (tagMatch) {
          const tagName = tagMatch[1];
          if (isKnownTag(tagName)) {
            result += line[i];
            i++;
            continue;
          }
        }
        result += '&lt;';
        i++;
        continue;
      }

      result += '&lt;';
      i++;
      continue;
    }

    result += line[i];
    i++;
  }

  return result;
}

function isJsxLine(line) {
  const trimmed = line.trim();
  if (/^<[A-Z]/.test(trimmed)) return true;
  if (/^<\/[A-Z]/.test(trimmed)) return true;
  return false;
}

const HTML_TAGS = new Set([
  'a', 'abbr', 'address', 'area', 'article', 'aside', 'audio',
  'b', 'base', 'bdi', 'bdo', 'blockquote', 'body', 'br', 'button',
  'canvas', 'caption', 'cite', 'code', 'col', 'colgroup',
  'data', 'datalist', 'dd', 'del', 'details', 'dfn', 'dialog', 'div', 'dl', 'dt',
  'em', 'embed',
  'fieldset', 'figcaption', 'figure', 'footer', 'form',
  'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'head', 'header', 'hgroup', 'hr', 'html',
  'i', 'iframe', 'img', 'input', 'ins',
  'kbd',
  'label', 'legend', 'li', 'link',
  'main', 'map', 'mark', 'menu', 'meta', 'meter',
  'nav', 'noscript',
  'object', 'ol', 'optgroup', 'option', 'output',
  'p', 'param', 'picture', 'pre', 'progress',
  'q',
  'rp', 'rt', 'ruby',
  's', 'samp', 'script', 'section', 'select', 'slot', 'small', 'source',
  'span', 'strong', 'style', 'sub', 'summary', 'sup',
  'table', 'tbody', 'td', 'template', 'textarea', 'tfoot', 'th', 'thead',
  'time', 'title', 'tr', 'track',
  'u', 'ul',
  'var', 'video',
  'wbr',
]);

const JSX_COMPONENTS = new Set([
  'StatusBadge', 'DateBadge', 'DomainBadge', 'PriorityBadge', 'SeverityBadge',
  'RFCLevelBadge', 'RequirementBox', 'Field', 'FieldGroup',
  'Tabs', 'TabItem', 'Admonition',
]);

function isKnownTag(tagName) {
  if (HTML_TAGS.has(tagName.toLowerCase())) return true;
  if (JSX_COMPONENTS.has(tagName)) return true;
  return false;
}

// ============================================================================
// Graph Building (from graph-data.js, using lib-artifact-transforms)
// ============================================================================

const nodeId = (id) => id.replace(/[^A-Za-z0-9_]/g, '_');

const nodeLabel = (n) =>
  (n.title || n.id)
    .replace(/^(?:ADR|SPEC)-\d+:\s*/, '')
    .replace(/"/g, '\\"');

function extractTitle(text) {
  const m = text.match(/^#\s+(.+?)\s*$/m);
  return m ? m[1] : '';
}

function buildGraph({ adrsSource, specsSource }) {
  const nodes = {};
  const edges = [];

  // /i: ADR files are named adr-0001-... in some repos and ADR-0001-... in
  // others. Case-sensitive here meant zero ADR nodes in the graph, hence no
  // edges and no mini-DAGs, on every lowercase-naming repo.
  const adrFileRe = /^ADR-(\d{4})/i;
  if (fs.existsSync(adrsSource)) {
    for (const f of fs.readdirSync(adrsSource).sort()) {
      if (!f.endsWith('.md')) continue;
      const m = f.match(adrFileRe);
      if (!m) continue;
      const id = `ADR-${m[1]}`;
      const text = fs.readFileSync(path.join(adrsSource, f), 'utf-8');
      const { metadata } = parseFrontmatter(text);
      const title = extractTitle(text);
      nodes[id] = { id, kind: 'adr', title, path: path.join(adrsSource, f) };
      ingestEdges(edges, id, metadata, ADR_EDGE_FIELDS);
    }
  }

  if (fs.existsSync(specsSource)) {
    for (const dir of fs.readdirSync(specsSource).sort()) {
      const specPath = path.join(specsSource, dir, 'spec.md');
      if (!fs.existsSync(specPath)) continue;
      const text = fs.readFileSync(specPath, 'utf-8');
      const titleMatch = text.match(/^#\s+(SPEC-\d{4})\b/m);
      if (!titleMatch) continue;
      const id = titleMatch[1];
      const { metadata } = parseFrontmatter(text);
      const title = extractTitle(text);
      nodes[id] = { id, kind: 'spec', title, path: specPath, dir };
      ingestEdges(edges, id, metadata, SPEC_EDGE_FIELDS);
    }
  }

  // Derive inverse edges
  const authoredPairs = new Set(edges.map((e) => `${e.source}|${e.target}|${e.type}`));
  const derived = [];
  for (const e of edges) {
    const inv = INVERSE_OF[e.type];
    if (!inv) continue;
    if (!nodes[e.target]) continue;
    if (e.type === 'related' && authoredPairs.has(`${e.target}|${e.source}|related`)) continue;
    derived.push({ source: e.target, target: e.source, type: inv, derived: true });
  }
  for (const e of edges) e.derived = false;
  edges.push(...derived);

  // Orphans
  const orphanAdrs = [];
  const orphanSpecs = [];
  for (const id of Object.keys(nodes).sort()) {
    const n = nodes[id];
    if (n.kind === 'adr') {
      const hasSpecImpl = edges.some(
        (e) => e.target === id && e.type === 'implements' && !e.derived
      );
      if (!hasSpecImpl) orphanAdrs.push(id);
    }
    if (n.kind === 'spec') {
      const hasGoverning = edges.some(
        (e) =>
          e.target === id &&
          ((e.type === 'governs' && !e.derived) || e.type === 'implemented-by')
      );
      if (!hasGoverning) orphanSpecs.push(id);
    }
  }

  return { nodes, edges, orphanAdrs, orphanSpecs };
}

function ingestEdges(edges, sourceId, metadata, allowed) {
  for (const field of allowed) {
    const value = metadata[field];
    if (!Array.isArray(value)) continue;
    for (const target of value) {
      const t = String(target).trim();
      if (!t) continue;
      edges.push({ source: sourceId, target: t, type: field });
    }
  }
}

function renderFullMermaid({ nodes, edges }) {
  const lines = ['flowchart TB'];
  const seen = new Set();

  for (const id of Object.keys(nodes).sort()) {
    const n = nodes[id];
    if (seen.has(id)) continue;
    seen.add(id);
    lines.push(`  ${nodeId(id)}["${nodeLabel(n)}"]`);
  }
  for (const e of edges) {
    if (e.derived) continue;
    if (!nodes[e.source] || !nodes[e.target]) continue;
    const arrow = '-->';
    const label = e.type;
    lines.push(`  ${nodeId(e.source)} ${arrow}|"${label}"| ${nodeId(e.target)}`);
  }
  return lines.join('\n');
}

function renderNeighborMermaid(targetId, { nodes, edges }) {
  if (!nodes[targetId]) return null;
  const lines = ['flowchart TB'];
  const neighborhood = new Set([targetId]);
  for (const e of edges) {
    if (e.source === targetId || e.target === targetId) {
      neighborhood.add(e.source);
      neighborhood.add(e.target);
    }
  }
  if (neighborhood.size <= 1) return null;
  for (const id of [...neighborhood].sort()) {
    if (!nodes[id]) continue;
    lines.push(`  ${nodeId(id)}["${nodeLabel(nodes[id])}"]`);
  }
  for (const e of edges) {
    if (e.source !== targetId && e.target !== targetId) continue;
    if (!nodes[e.source] || !nodes[e.target]) continue;
    const arrow = e.derived ? '-.->' : '-->';
    const label = e.derived ? `${e.type} (derived)` : e.type;
    lines.push(`  ${nodeId(e.source)} ${arrow}|"${label}"| ${nodeId(e.target)}`);
  }
  return lines.join('\n');
}

// ADR-0023 and SPEC-0018 are the SDD plugin's *own* artifacts describing the
// frontmatter DAG. They exist in the repo this plugin was extracted from, and
// in almost no repo that consumes it — harness has ADR-0001..0014 and no
// artifact-graph spec, so linking them unconditionally put two dead links on
// every ADR page (28 here). onBrokenLinks is 'warn', so the build stayed green
// and nobody noticed.
//
// Cite them only when this repo actually has them; otherwise name them as
// plain text. The load-bearing half of the sentence is the /sdd:graph hint,
// which is true everywhere.
function citeGraphArtifacts(graph) {
  const adr = Object.values(graph.nodes || {}).find(
    (n) => n.kind === 'adr' && /frontmatter-dag/i.test(path.basename(n.path || ''))
  );
  const spec = (graph.nodes || {})['SPEC-0018'];

  const adrRef = adr ? `[${adr.id}](/decisions/${path.basename(adr.path, '.md')})` : 'ADR-0023';
  const specRef = spec && spec.dir ? `[SPEC-0018](/specs/${spec.dir}/spec)` : 'SPEC-0018';
  return `${adrRef} / ${specRef}`;
}

function buildMiniDagSection(artifactId, graph) {
  if (!artifactId) return '';
  const mermaid = renderNeighborMermaid(artifactId, graph);
  if (!mermaid) return '';
  return [
    '',
    '',
    '## Related Artifacts',
    '',
    `Direct relationships declared in YAML frontmatter (per ${citeGraphArtifacts(graph)}). Run \`/sdd:graph chain ${artifactId}\` for the transitive view.`,
    '',
    '```mermaid',
    mermaid,
    '```',
    '',
  ].join('\n');
}

// ============================================================================
// Spec Mapping (from build-spec-mapping.js)
// ============================================================================

function buildSpecMapping(specsSource) {
  const mapping = {};
  const emojis = {};

  if (!fs.existsSync(specsSource)) {
    return { mapping, emojis };
  }

  const domains = fs.readdirSync(specsSource);

  for (const domain of domains) {
    const domainPath = path.join(specsSource, domain);
    if (!fs.statSync(domainPath).isDirectory()) continue;

    const specPath = path.join(domainPath, 'spec.md');
    if (!fs.existsSync(specPath)) continue;

    const content = fs.readFileSync(specPath, 'utf-8');

    const prefixes = new Set();

    const h1Match = content.match(/^#\s+([A-Z]+)-\d{4}:/m);
    if (h1Match) {
      prefixes.add(h1Match[1]);
    }

    const tableMatches = content.matchAll(/\|\s*([A-Z]+)-\d{3,4}\s*\|/g);
    for (const match of tableMatches) {
      prefixes.add(match[1]);
    }

    const headingMatches = content.matchAll(/###\s+Requirement:.*?([A-Z]+)-\d{3,4}/g);
    for (const match of headingMatches) {
      prefixes.add(match[1]);
    }

    for (const prefix of prefixes) {
      mapping[prefix] = `/specs/${domain}/spec`;
    }
  }

  return { mapping, emojis };
}

// ============================================================================
// Transform Utilities (from transform-utils.js)
// ============================================================================

function isCodeFence(line) {
  const trimmed = line.trimStart();
  return /^(`{3,}|~{3,})/.test(trimmed);
}

function buildAdrMapping(adrsSource) {
  const mapping = {};
  if (!fs.existsSync(adrsSource)) return mapping;

  const files = fs.readdirSync(adrsSource);
  for (const file of files) {
    if (!file.endsWith('.md')) continue;
    if (file === '0000-template.md' || file === 'README.md') continue;

    const match = file.match(/^(?:ADR-)?(\d{4})-/i);
    if (match) {
      const number = match[1];
      const slug = file.replace(/\.md$/, '');
      mapping[number] = `/decisions/${slug}`;
    }
  }
  return mapping;
}

function transformRfc2119Keywords(content) {
  const keywordPattern = /\b(MUST NOT|SHALL NOT|SHOULD NOT|MUST|SHALL|REQUIRED|SHOULD|RECOMMENDED|MAY|OPTIONAL)\b/g;
  const keywordClasses = {
    'MUST NOT': 'must', 'SHALL NOT': 'shall', 'SHOULD NOT': 'should',
    'MUST': 'must', 'SHALL': 'shall', 'REQUIRED': 'required',
    'SHOULD': 'should', 'RECOMMENDED': 'recommended',
    'MAY': 'may', 'OPTIONAL': 'optional',
  };

  const lines = content.split('\n');
  let inCodeBlock = false;

  return lines.map(line => {
    if (isCodeFence(line)) { inCodeBlock = !inCodeBlock; return line; }
    if (inCodeBlock || line.startsWith('#') || line.startsWith('    ')) return line;
    if (line.match(/^`[^`]+`$/)) return line;

    const parts = line.split(/(`[^`]+`)/);
    return parts.map(part => {
      if (part.startsWith('`') && part.endsWith('`')) return part;
      return part.replace(keywordPattern, (match) => {
        const cls = keywordClasses[match];
        return `<span className="rfc-keyword ${cls}">${match}</span>`;
      });
    }).join('');
  }).join('\n');
}

// Spans within a single line that must never be auto-linkified.
//
// The reference transforms below turn a bare `ADR-0006` / `SPEC-0004` mention
// into an <a>. Three places that is wrong, and all three occur in this repo's
// ADRs:
//
//   `ADR-0006`                      inline code — a literal, not a reference
//   [ADR-0006](adr-0006-....md)     already a link; wrapping the label yields
//                                   <a><a>…</a></a>, which is invalid HTML and
//                                   which the SSG's minifier rejects outright
//   <a … >ADR-0006</a>              already transformed on an earlier pass
//
// The nesting case was dormant only because adrMapping was empty (see
// buildAdrMapping): with no entries, nothing was ever linkified. Populating the
// mapping made every `[ADR-NNNN](...)` in a body produce nested anchors, so
// this guard is a prerequisite for that fix rather than an independent nicety.
function protectedRanges(line) {
  const ranges = [];
  for (const re of [/`[^`]*`/g, /\[[^\]]*\]\([^)]*\)/g, /<[^>]+>/g]) {
    let m;
    while ((m = re.exec(line)) !== null) ranges.push([m.index, m.index + m[0].length]);
  }
  return ranges;
}

// String.prototype.replace passes (match, ...groups, offset, whole), so the
// offset is always the second-to-last argument regardless of group count.
function matchOffset(args) {
  return args[args.length - 2];
}

function isProtected(ranges, start, end) {
  return ranges.some(([from, to]) => start >= from && end <= to);
}

function transformSpecReferences(content, { specMapping, specEmojis, baseUrl }) {
  const specPattern = /\b([A-Z]+)-(\d{3,4})\b/g;
  const lines = content.split('\n');
  let inCodeBlock = false;

  return lines.map(line => {
    if (isCodeFence(line)) { inCodeBlock = !inCodeBlock; return line; }
    if (inCodeBlock || line.startsWith('#')) return line;
    if (line.trim().startsWith('<') && !line.includes('className="rfc-keyword')) return line;

    const ranges = protectedRanges(line);
    return line.replace(specPattern, (match, prefix, number, ...rest) => {
      const offset = matchOffset([match, prefix, number, ...rest]);
      if (isProtected(ranges, offset, offset + match.length)) return match;
      const specPath = specMapping[prefix];
      const emoji = specEmojis[prefix];
      if (!specPath) return match;
      const displayText = emoji ? `${emoji} ${match}` : match;
      const anchorId = match.toLowerCase();
      return `<a href="${baseUrl}${specPath}#${anchorId}" className="rfc-ref">${displayText}</a>`;
    });
  }).join('\n');
}

function transformAdrReferences(content, { adrMapping, adrEmoji, baseUrl }) {
  const adrPattern = /\bADR-(\d{4})\b/g;
  const lines = content.split('\n');
  let inCodeBlock = false;

  return lines.map(line => {
    if (isCodeFence(line)) { inCodeBlock = !inCodeBlock; return line; }
    if (inCodeBlock || line.startsWith('#')) return line;
    if (line.trim().startsWith('<') && !line.includes('className="rfc-keyword') && !line.includes('className="rfc-ref')) return line;

    const ranges = protectedRanges(line);
    return line.replace(adrPattern, (match, number, ...rest) => {
      const offset = matchOffset([match, number, ...rest]);
      if (isProtected(ranges, offset, offset + match.length)) return match;
      const adrPath = adrMapping[number];
      if (!adrPath) return match;
      const displayText = `${adrEmoji} ${match}`;
      return `<a href="${baseUrl}${adrPath}" className="rfc-ref">${displayText}</a>`;
    });
  }).join('\n');
}

function fixMarkdownLinks(content) {
  return content.replace(/\]\(((?!https?:\/\/)[^)]*?)\.md(#[^)]*?)?\)/g, ']($1$2)');
}

// ============================================================================
// ADR Transformation (from transform-adrs.js)
// ============================================================================

function escapeYaml(str) {
  return str.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

// Frontmatter values are whatever YAML says they are, not necessarily strings:
// `decision-makers: [joestump]` is an array, and an unquoted `date: 2026-07-27`
// is a Date under some parsers. Coerce before touching string methods, or the
// build dies with the memorable-but-unhelpful "str.replace is not a function".
//
// This is not hypothetical caution — every ADR in this repo carries
// `decision-makers: [<name>]`. The crash was merely unreachable while
// isNumberedAdr was failing to match lowercase filenames and suppressing the
// whole badge header (see transformAdr). Fixing that without this would trade a
// silent omission for a hard build failure.
function toDisplayString(value) {
  if (value === null || value === undefined) return '';
  if (Array.isArray(value)) return value.join(', ');
  // Render as YYYY-MM-DD; the source frontmatter is date-only, and toISOString
  // would otherwise append a midnight-UTC time the author never wrote.
  if (value instanceof Date) return value.toISOString().slice(0, 10);
  return String(value);
}

function escapeJsxAttr(value) {
  return toDisplayString(value)
    .replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function extractMetadataAdr(content) {
  let status = null;
  let date = null;
  let dm = null;

  const fmMatch = content.match(/^---\n([\s\S]*?)\n---/);
  if (fmMatch) {
    const { metadata } = parseFrontmatter(content);
    status = metadata.status || null;
    date = metadata.date || null;
    dm = metadata['decision-makers'] || null;
  }

  if (!status) status = 'unknown';
  if (!date) date = 'unknown';
  if (!dm) dm = 'unknown';

  return { status, date, dm };
}

function escapeBidirectionalArrows(content) {
  return content.replace(/<-+>/g, (match) => {
    return match.replace(/</g, '&lt;').replace(/>/g, '&gt;');
  });
}

function transformConsequenceKeywords(content) {
  const lines = content.split('\n');
  let inCodeBlock = false;

  return lines.map(line => {
    const trimmed = line.trimStart();
    if (/^(`{3,}|~{3,})/.test(trimmed)) { inCodeBlock = !inCodeBlock; return line; }
    if (inCodeBlock) return line;
    return line.replace(/^(\s*[\*\-]\s+)(Good|Bad|Neutral|Meh|Okay)(,)/i, (match, prefix, keyword, comma) => {
      const normalizedKeyword = keyword.charAt(0).toUpperCase() + keyword.slice(1).toLowerCase();
      const cssClass = normalizedKeyword.toLowerCase();
      return `${prefix}<span className="consequence-keyword ${cssClass}">${normalizedKeyword}</span>${comma}`;
    });
  }).join('\n');
}

function fixCrossSectionPaths(content) {
  return content.replace(/\]\(\.\.\/openspec\/specs\//g, '](../specs/');
}

function transformAdr(srcPath, destPath, fileName, { specMapping, specEmojis, baseUrl, adrMapping, graph }) {
  let content = fs.readFileSync(srcPath, 'utf-8');

  if (fileName === '0000-template.md' || fileName === 'README.md') return;

  const isNumberedAdr = /^(?:ADR-)?\d{4}-/i.test(fileName);
  const title = extractTitle(content);
  const { status, date, dm } = extractMetadataAdr(content);

  const contentWithoutFrontmatter = content.replace(/^---[\s\S]*?---/, '').trim();

  let escapedContent = fixMarkdownLinks(contentWithoutFrontmatter);
  escapedContent = fixCrossSectionPaths(escapedContent);
  escapedContent = escapeBidirectionalArrows(escapedContent);
  escapedContent = transformRfc2119Keywords(escapedContent);
  escapedContent = transformSpecReferences(escapedContent, { specMapping, specEmojis, baseUrl });
  escapedContent = transformAdrReferences(escapedContent, { adrMapping, adrEmoji: '📝', baseUrl });
  escapedContent = transformConsequenceKeywords(escapedContent);

  const slug = fileName.replace(/\.md$/, '');

  let sidebarLabel;
  if (isNumberedAdr) {
    const adrNum = fileName.match(/^(?:ADR-)?(\d{4})-/i)[1];
    const titleWithoutAdr = title.replace(/^ADR-\d+:\s*/i, '');
    sidebarLabel = `ADR-${adrNum}: ${titleWithoutAdr}`;
  } else {
    sidebarLabel = title;
  }

  const badgeHeader = isNumberedAdr ? `
<FieldGroup>
  <Field label="Status">
    <StatusBadge status="${escapeJsxAttr(status.toUpperCase())}" />
  </Field>
  <Field label="Date">
    <DateBadge date="${escapeJsxAttr(date)}" />
  </Field>
  <Field label="Decision Makers">${escapeJsxAttr(dm)}</Field>
</FieldGroup>
` : '';

  const isStricken = ['deprecated', 'superseded'].includes(status.toLowerCase());
  const sidebarClassName = isStricken ? '\nsidebar_class_name: adr-struck' : '';

  const frontmatter = `---\ntitle: "${escapeYaml(title)}"
sidebar_label: "${escapeYaml(sidebarLabel)}"
slug: /decisions/${slug}${sidebarClassName}
---
${badgeHeader}
`;

  const adrIdMatch = fileName.match(/^(ADR-\d{4})/i);
  const artifactId = adrIdMatch ? adrIdMatch[1].toUpperCase() : null;
  const miniDag = buildMiniDagSection(artifactId, graph);

  fs.mkdirSync(path.dirname(destPath), { recursive: true });
  fs.writeFileSync(destPath, frontmatter + escapeMdxUnsafe(escapedContent) + miniDag);
}

// ============================================================================
// OpenSpec Transformation (from transform-openspecs.js)
// ============================================================================

function extractMetadataSpec(content, fileType) {
  let status = null;
  let date = null;

  const fmMatch = content.match(/^---\n([\s\S]*?)\n---/);
  if (fmMatch) {
    const { metadata } = parseFrontmatter(content);
    status = metadata.status || null;
    date = metadata.date || null;
    content = content.slice(fmMatch[0].length).replace(/^\n+/, '');
  }

  if (!status) status = fileType === 'spec' ? 'active' : 'draft';
  if (!date) date = 'unknown';

  return { status, date, content };
}

function buildDomainConfig(specsSource) {
  const config = {};
  if (!fs.existsSync(specsSource)) return config;

  const domains = fs.readdirSync(specsSource)
    .filter(d => fs.statSync(path.join(specsSource, d)).isDirectory())
    .sort();

  domains.forEach((domain, index) => {
    let prefix = domain.toUpperCase().replace(/-/g, '').slice(0, 6);
    const specPath = path.join(specsSource, domain, 'spec.md');
    if (fs.existsSync(specPath)) {
      const content = fs.readFileSync(specPath, 'utf-8');
      const specMatch = content.match(/^#\s+SPEC-\d+:\s+(.+)$/m);
      if (specMatch) {
        const label = specMatch[1].trim();
        config[domain] = { order: index + 1, label: label };
        return;
      }
    }

    const label = domain
      .split('-')
      .map(w => w.charAt(0).toUpperCase() + w.slice(1))
      .join(' ');
    config[domain] = { order: index + 1, label: label };
  });

  return config;
}

function fixRelativePaths(content) {
  content = content.replace(/\]\(\.\.\/\.\.\/\.\.\/decisions\//g, '](../../decisions/');
  content = content.replace(/\]\(\.\.\/\.\.\/\.\.\/docs\//g, '](../../');
  return content;
}

function transformRequirementTables(content) {
  const tableRegex = /\| ID \| Requirement \|\n\|[-| ]+\|\n((?:\| [A-Z0-9-]+ \| .* \|\n)+)/g;

  return content.replace(tableRegex, (match, rows) => {
    const rowRegex = /\| ([A-Z0-9-]+) \| (.*) \|/g;
    let result = '';
    let rowMatch;

    while ((rowMatch = rowRegex.exec(rows)) !== null) {
      const id = rowMatch[1].trim();
      const text = rowMatch[2].trim();
      result += `<RequirementBox id="${id}">\n\n${text}\n\n</RequirementBox>\n\n`;
    }

    return result;
  });
}

function transformSpec(srcPath, destPath, domain, fileType, domainConfig, flat, { specMapping, specEmojis, baseUrl, adrMapping, graph }) {
  let content = fs.readFileSync(srcPath, 'utf-8');
  const config = domainConfig[domain] || { order: 99, label: domain };

  const metadata = extractMetadataSpec(content, fileType);
  content = metadata.content;
  const title = extractTitle(content);

  content = fixRelativePaths(content);
  content = fixMarkdownLinks(content);

  if (fileType === 'spec') {
    content = transformRequirementTables(content);
  }

  content = transformRfc2119Keywords(content);
  content = transformSpecReferences(content, { specMapping, specEmojis, baseUrl });
  content = transformAdrReferences(content, { adrMapping, adrEmoji: '📝', baseUrl });

  const sidebarLabel = flat ? config.label : (fileType === 'spec' ? 'Specification' : 'Design');
  const sidebarPosition = flat ? config.order : (fileType === 'spec' ? 1 : 2);

  const metadataHeader = `
<FieldGroup>
  <Field label="Status">
    <StatusBadge status="${escapeJsxAttr(metadata.status.toUpperCase())}" />
  </Field>
  <Field label="Date">
    <DateBadge date="${escapeJsxAttr(metadata.date)}" />
  </Field>
  <Field label="Domain">
    <DomainBadge domain="${escapeJsxAttr(config.label)}" />
  </Field>
</FieldGroup>
`;

  const frontmatter = `---
title: "${escapeYaml(title)}"
sidebar_label: "${escapeYaml(sidebarLabel)}"
sidebar_position: ${sidebarPosition}
---

${metadataHeader}

`;

  const specDomainToId = {};
  for (const node of Object.values(graph.nodes)) {
    if (node.kind === 'spec' && node.dir) {
      specDomainToId[node.dir] = node.id;
    }
  }
  const artifactId = specDomainToId[domain];
  const miniDag = buildMiniDagSection(artifactId, graph);

  fs.mkdirSync(path.dirname(destPath), { recursive: true });
  fs.writeFileSync(destPath, frontmatter + escapeMdxUnsafe(content) + miniDag);
}

function generateCategoryJson(destDir, domain, domainConfig) {
  const config = domainConfig[domain] || { order: 99, label: domain };

  const categoryData = {
    label: config.label,
    position: config.order,
    link: {
      type: 'generated-index',
      description: `${config.label} specifications and design documents.`
    }
  };

  const categoryPath = path.join(destDir, '_category_.json');
  fs.writeFileSync(categoryPath, JSON.stringify(categoryData, null, 2));
}

// ============================================================================
// Index Page Generation (from generate-index.js)
// ============================================================================

function renderHierarchySection({ kind, kindPlural }, graph, baseUrl) {
  if (!graph.nodes || Object.keys(graph.nodes).length === 0) return '';

  const filteredNodes = {};
  for (const [id, node] of Object.entries(graph.nodes)) {
    if (node.kind === kind) filteredNodes[id] = node;
  }
  if (Object.keys(filteredNodes).length === 0) return '';

  const filteredEdges = graph.edges.filter(
    (e) => filteredNodes[e.source] && filteredNodes[e.target]
  );

  const mermaid = renderFullMermaid({ nodes: filteredNodes, edges: filteredEdges });
  return [
    '',
    '## Hierarchy',
    '',
    `Authored relationships among ${kindPlural} in this project (per ${citeGraphArtifacts(graph)}). Cross-kind links (e.g., which ADR a spec implements) appear in each artifact's per-page "Related Artifacts" mini-DAG.`,
    '',
    '```mermaid',
    mermaid,
    '```',
    '',
  ].join('\n');
}

function countAdrs(adrsSource) {
  if (!fs.existsSync(adrsSource)) return 0;
  return fs.readdirSync(adrsSource)
    .filter(f => f.endsWith('.md') && f !== '0000-template.md' && f !== 'README.md')
    .length;
}

function countSpecs(specsSource) {
  if (!fs.existsSync(specsSource)) return 0;
  return fs.readdirSync(specsSource)
    .filter(d => {
      const dirPath = path.join(specsSource, d);
      return fs.statSync(dirPath).isDirectory() && fs.existsSync(path.join(dirPath, 'spec.md'));
    })
    .length;
}

function generateSpecsIndex(specsSource, docsDest, graph, baseUrl) {
  if (!fs.existsSync(specsSource)) return;

  const specsDest = path.join(docsDest, 'specs');
  fs.mkdirSync(specsDest, { recursive: true });

  const domains = fs.readdirSync(specsSource)
    .filter(d => fs.statSync(path.join(specsSource, d)).isDirectory())
    .sort();

  const rows = [];
  for (const domain of domains) {
    const domainPath = path.join(specsSource, domain);
    const hasSpec = fs.existsSync(path.join(domainPath, 'spec.md'));
    const hasDesign = fs.existsSync(path.join(domainPath, 'design.md'));

    if (!hasSpec && !hasDesign) continue;

    let label = domain.split('-').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ');
    if (hasSpec) {
      const content = fs.readFileSync(path.join(domainPath, 'spec.md'), 'utf-8');
      const titleMatch = content.match(/^#\s+SPEC-\d+:\s+(.+)$/m);
      if (titleMatch) label = titleMatch[1].trim();
    }

    let docs;
    if (hasSpec && hasDesign) {
      docs = `[Specification](./${domain}/spec) / [Design](./${domain}/design)`;
    } else if (hasSpec) {
      docs = `[Specification](./${domain})`;
    } else {
      docs = `[Design](./${domain})`;
    }

    rows.push(`| ${label} | ${docs} |`);
  }

  if (rows.length === 0) return;

  const content = `---
title: "Specifications"
sidebar_label: "Overview"
sidebar_position: 0
---

# Specifications

| Component | Documents |
|-----------|-----------|
${rows.join('\n')}
${renderHierarchySection({ kind: 'spec', kindPlural: 'specs' }, graph, baseUrl)}`;

  fs.writeFileSync(path.join(specsDest, 'index.mdx'), content);
}

function generateDecisionsIndex(adrsSource, docsDest, graph, baseUrl) {
  if (!fs.existsSync(adrsSource)) return;

  const decisionsDest = path.join(docsDest, 'decisions');
  fs.mkdirSync(decisionsDest, { recursive: true });

  const files = fs.readdirSync(adrsSource)
    .filter(f => f.endsWith('.md') && f !== '0000-template.md' && f !== 'README.md')
    .sort();

  const strike = (text, status) =>
    ['deprecated', 'superseded'].includes(status.toLowerCase()) ? `~~${text}~~` : text;

  const rows = [];
  for (const file of files) {
    const content = fs.readFileSync(path.join(adrsSource, file), 'utf-8');

    const idMatch = file.match(/^(ADR-\d{4})/i);
    const id = idMatch ? idMatch[1].toUpperCase() : file.replace(/\.md$/, '');
    const titleMatch = content.match(/^#\s+(?:ADR-\d+:\s*)?(.+)$/m);
    const title = titleMatch ? titleMatch[1].trim() : id;

    const fmMatch = content.match(/^---\n([\s\S]*?)\n---/);
    let status = 'unknown';
    if (fmMatch) {
      const { metadata } = parseFrontmatter(content);
      status = metadata.status || 'unknown';
    }

    const slug = file.replace(/\.md$/, '');
    rows.push(`| ${strike(id, status)} | ${strike(`[${title}](./${slug})`, status)} | \`${status}\` |`);
  }

  if (rows.length === 0) return;

  const content = `---
title: "Architecture Decisions"
sidebar_label: "Overview"
sidebar_position: 0
---

# Architecture Decisions

| ID | Title | Status |
|----|-------|--------|
${rows.join('\n')}
${renderHierarchySection({ kind: 'adr', kindPlural: 'ADRs' }, graph, baseUrl)}`;

  fs.writeFileSync(path.join(decisionsDest, 'index.mdx'), content);
}

// Homepage hero + feature tiles. The tiles describe the project itself (not
// the ADR/spec tree), so they live here as data; the ADR/spec counts below
// stay derived from the source tree.
const HOMEPAGE_TAGLINE = 'systemctl for your agents';
const HOMEPAGE_FEATURES = [
  {
    icon: '🖥️',
    title: 'Keyboard-driven dashboard',
    description: 'Open the cockpit TUI and hop between every harness with live terminal previews — no args, just harness.',
    href: '/usage/tui',
  },
  {
    icon: '♻️',
    title: 'Real supervision',
    description: 'Restart policies, escalating crash-loop backoff and state persistence — harnesses survive client disconnects and daemon restarts.',
    href: '/usage/supervision',
  },
  {
    icon: '🤖',
    title: 'Agent adapters',
    description: 'First-class Claude Code, Crush and Codex support: prompt one-shots, model selection, turn budgets, trajectory harvesting.',
    href: '/usage/configuration#agent-adapters',
  },
  {
    icon: '⏰',
    title: 'Scheduled one-shots',
    description: 'Give a prompt harness a cron schedule and the daemon fires it on cadence — overlapping runs skip, never stack.',
    href: '/usage/configuration#scheduled-one-shots',
  },
  {
    icon: '📦',
    title: 'Project compose',
    description: 'A repo-root harness.toml gives every checkout its own fleet: harness up / down / ps with project-scoped names.',
    href: '/usage/projects',
  },
  {
    icon: '🔒',
    title: 'Remote over SSH',
    description: 'The same dashboard over the network via Wish — public-key auth only, per-key read-only scoping, no password path.',
    href: '/usage/remote',
  },
];

function generateMainIndex(adrsSource, specsSource, docsDest, projectTitle, baseUrl) {
  const adrCount = countAdrs(adrsSource);
  const specCount = countSpecs(specsSource);

  const esc = (s) => s.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
  const tiles = HOMEPAGE_FEATURES.map(
    (f) => `<a className="feature-tile" href="${baseUrl}${f.href}">
  <div className="feature-tile__icon">${f.icon}</div>
  <div className="feature-tile__title">${esc(f.title)}</div>
  <div className="feature-tile__description">${esc(f.description)}</div>
</a>`,
  ).join('\n');

  const content = `---
title: "${esc(projectTitle)}"
slug: /
hide_title: true
hide_table_of_contents: true
---

<div className="homepage">
<div className="homepage-hero">
  <h1 className="homepage-hero__title">${esc(projectTitle)}</h1>
  <p className="homepage-hero__subtitle">${esc(HOMEPAGE_TAGLINE)} — supervise, attach to, and hop between long-running agent CLIs, REPLs and watchers from one Go binary.</p>
</div>

<div className="feature-tiles">
${tiles}
</div>

${adrCount > 0 || specCount > 0 ? `<div className="homepage__footnote">

## Under the hood

This project has **${adrCount}** architecture decision${adrCount !== 1 ? 's' : ''} and **${specCount}** specification${specCount !== 1 ? 's' : ''} recording why every behavior above is the way it is.

[Browse Architecture Decisions →](${baseUrl}/decisions) · [Browse Specifications →](${baseUrl}/specs)

</div>
` : 'No architecture artifacts found yet.'}

</div>`;

  fs.mkdirSync(docsDest, { recursive: true });
  fs.writeFileSync(path.join(docsDest, 'index.mdx'), content);
}

// ============================================================================
// Graph Page Generation (from generate-graph.js)
// ============================================================================

function generateGraphPage(graph, docsDest) {
  const { nodes, edges, orphanAdrs, orphanSpecs } = graph;

  const adrCount = Object.values(nodes).filter((n) => n.kind === 'adr').length;
  const specCount = Object.values(nodes).filter((n) => n.kind === 'spec').length;
  const authoredEdges = edges.filter((e) => !e.derived);
  const derivedEdges = edges.filter((e) => e.derived);

  if (adrCount + specCount === 0) {
    return;
  }

  const mermaid = renderFullMermaid(graph);

  const stripIdPrefix = (title, id) =>
    (title || '').replace(new RegExp(`^${id}:\\s*`), '');
  const orphanAdrSection = orphanAdrs.length
    ? `| ADR | Title |\n|-----|-------|\n${orphanAdrs
        .map((id) => `| ${id} | ${stripIdPrefix(nodes[id] && nodes[id].title, id)} |`)
        .join('\n')}`
    : '_No orphan ADRs — every ADR has at least one implementing spec._';
  const orphanSpecSection = orphanSpecs.length
    ? `| Spec | Title |\n|------|-------|\n${orphanSpecs
        .map((id) => `| ${id} | ${stripIdPrefix(nodes[id] && nodes[id].title, id)} |`)
        .join('\n')}`
    : '_No orphan specs — every spec is governed by at least one ADR._';

  const content = `---
title: "Architecture Graph"
sidebar_label: "Graph"
sidebar_position: 1
---

# Architecture Graph

The artifact graph captures explicit relationships between ADRs and specs declared in YAML frontmatter (per ${citeGraphArtifacts(graph)}). Edges describe \`supersedes\`, \`extends\`, \`enables\`, \`governs\`, \`implements\`, \`requires\`, and \`related\` relationships between artifacts. The page below reflects the authored edges only; derived inverses (\`governed-by\`, \`implemented-by\`, etc.) are computed at query time by the \`/sdd:graph\` skill.

## Stats

| Metric | Count |
|--------|-------|
| ADRs | ${adrCount} |
| Specs | ${specCount} |
| Authored edges | ${authoredEdges.length} |
| Derived edges (computed) | ${derivedEdges.length} |
| Orphan ADRs (no implementing spec) | ${orphanAdrs.length} |
| Orphan specs (no governing ADR) | ${orphanSpecs.length} |

## Full graph

\`\`\`mermaid
${mermaid}
\`\`\`

## Orphan ADRs

ADRs that no spec declares \`implements:\` against. Add an \`implements: [ADR-XXXX]\` line to a spec's frontmatter (or run \`/sdd:graph backfill\`) to remove an ADR from this list.

${orphanAdrSection}

## Orphan specs

Specs that no ADR declares \`governs:\` against. (For specs whose source-code coverage is the relevant orphan signal, use \`/sdd:graph orphans\` directly — that walks source files for governing comments and is not reflected in this static page.)

${orphanSpecSection}

## Querying the graph

The static view above is generated at docs-build time. For interactive queries:

\`\`\`
/sdd:graph validate                  # full diagnostics
/sdd:graph impact ADR-XXXX           # what depends on this ADR
/sdd:graph ancestors SPEC-XXXX       # what this spec depends on
/sdd:graph chain SPEC-XXXX           # bidirectional view
/sdd:graph orphans                   # source files, specs, ADRs
/sdd:graph backfill                  # propose edges from prose
\`\`\`

JSON output (\`--json\`) is the stable contract for any future MCP, IDE plugin, or dashboard.
`;

  fs.mkdirSync(docsDest, { recursive: true });
  fs.writeFileSync(path.join(docsDest, 'graph.mdx'), content);
}

// ============================================================================
// Plugin Export
// ============================================================================

module.exports = function(context, options) {
  const { siteDir } = context;
  const opts = options || {};

  const adrsDir = opts.adrsDir || '../docs/adrs';
  const specsDir = opts.specsDir || '../docs/openspec/specs';
  const outputDir = opts.outputDir || '../docs-generated';

  const adrsSource = path.resolve(siteDir, adrsDir);
  const specsSource = path.resolve(siteDir, specsDir);
  const docsDest = path.resolve(siteDir, outputDir);

  // Ensure the output dir exists before @docusaurus/plugin-content-docs
  // validates its `path` option on a cold start (first build after clone).
  fs.mkdirSync(docsDest, { recursive: true });

  // Take baseUrl from the resolved site config, never by scraping the config
  // file. Docusaurus hands every plugin the fully-evaluated `siteConfig`, so
  // this is both the supported source and the only one that can be right.
  //
  // The previous implementation regex'd docusaurus.config.ts for
  // `baseUrl: '...'` and fell back to '' when it did not match. It never
  // matched: the config assigns `baseUrl: BASE_URL` from a constant (so that
  // CI can override it per host via DOCS_BASE_URL), and there is no string
  // literal on that line to capture. Every cross-reference chip was therefore
  // emitted at the host root — 94 links to /specs/... on a site served from
  // /harness/, each one a 404. The failure was invisible locally because
  // `npm run serve` and the dev server both mount at baseUrl anyway.
  //
  // Trailing slash is stripped because callers concatenate `${baseUrl}${path}`
  // where path already leads with '/'. Docusaurus guarantees siteConfig.baseUrl
  // both starts and ends with '/', so '/harness/' becomes '/harness' and the
  // root case '/' correctly becomes ''.
  const baseUrl = (context.siteConfig.baseUrl || '/').replace(/\/$/, '');

  const projectTitle = context.siteConfig.title || 'Architecture Documentation';

  return {
    name: 'sdd-content',

    async loadContent() {
      console.log('[sdd-content] Building documentation content...');

      // Build spec mapping and graph
      const { mapping: specMapping, emojis: specEmojis } = buildSpecMapping(specsSource);
      const graph = buildGraph({ adrsSource, specsSource });

      // Read optional user-provided emoji overrides
      const emojiOverridePath = path.resolve(siteDir, 'src/data/spec-emojis.json');
      if (fs.existsSync(emojiOverridePath)) {
        try {
          const userEmojis = JSON.parse(fs.readFileSync(emojiOverridePath, 'utf-8'));
          Object.assign(specEmojis, userEmojis);
        } catch (e) {
          // Ignore parsing errors
        }
      }

      const transformContext = {
        specMapping,
        specEmojis,
        baseUrl,
        adrMapping: buildAdrMapping(adrsSource),
        graph,
      };

      // Transform ADRs
      const ADRS_DEST = path.join(docsDest, 'decisions');
      if (fs.existsSync(ADRS_DEST)) {
        fs.rmSync(ADRS_DEST, { recursive: true });
      }
      fs.mkdirSync(ADRS_DEST, { recursive: true });
      fs.writeFileSync(path.join(ADRS_DEST, '_category_.json'), JSON.stringify({
        label: 'Architecture Decisions',
        position: 2,
      }, null, 2));

      if (fs.existsSync(adrsSource)) {
        const files = fs.readdirSync(adrsSource);
        for (const file of files) {
          if (!file.endsWith('.md')) continue;
          if (file === '0000-template.md' || file === 'README.md') continue;
          const srcPath = path.join(adrsSource, file);
          const destPath = path.join(ADRS_DEST, file.replace(/\.md$/, '.mdx'));
          transformAdr(srcPath, destPath, file, transformContext);
        }
        console.log(`  Transformed ADRs`);
      }

      // Transform OpenSpecs
      const SPECS_DEST = path.join(docsDest, 'specs');
      if (fs.existsSync(SPECS_DEST)) {
        fs.rmSync(SPECS_DEST, { recursive: true });
      }
      fs.mkdirSync(SPECS_DEST, { recursive: true });
      fs.writeFileSync(path.join(SPECS_DEST, '_category_.json'), JSON.stringify({
        label: 'Specifications',
        position: 1,
      }, null, 2));

      if (fs.existsSync(specsSource)) {
        const domainConfig = buildDomainConfig(specsSource);
        const domains = fs.readdirSync(specsSource);
        for (const domain of domains) {
          const domainPath = path.join(specsSource, domain);
          if (!fs.statSync(domainPath).isDirectory()) continue;

          const hasSpec = fs.existsSync(path.join(domainPath, 'spec.md'));
          const hasDesign = fs.existsSync(path.join(domainPath, 'design.md'));

          if (!hasSpec && !hasDesign) continue;

          if (hasSpec && hasDesign) {
            const destDomainPath = path.join(SPECS_DEST, domain);
            fs.mkdirSync(destDomainPath, { recursive: true });
            generateCategoryJson(destDomainPath, domain, domainConfig);

            transformSpec(path.join(domainPath, 'spec.md'), path.join(destDomainPath, 'spec.mdx'), domain, 'spec', domainConfig, false, transformContext);
            transformSpec(path.join(domainPath, 'design.md'), path.join(destDomainPath, 'design.mdx'), domain, 'design', domainConfig, false, transformContext);
          } else {
            const file = hasSpec ? 'spec.md' : 'design.md';
            const fileType = hasSpec ? 'spec' : 'design';
            const destPath = path.join(SPECS_DEST, `${domain}.mdx`);
            transformSpec(path.join(domainPath, file), destPath, domain, fileType, domainConfig, true, transformContext);
          }
        }
        console.log(`  Transformed OpenSpecs`);
      }

      // Generate index pages
      generateMainIndex(adrsSource, specsSource, docsDest, projectTitle, baseUrl);
      generateSpecsIndex(specsSource, docsDest, graph, baseUrl);
      generateDecisionsIndex(adrsSource, docsDest, graph, baseUrl);
      console.log(`  Generated index pages`);

      // Generate graph page
      generateGraphPage(graph, docsDest);
      console.log(`  Generated graph page`);

      console.log('[sdd-content] Documentation content build complete!');

      // Return undefined (MDX writing is a side effect, same as sync-spec-docs)
      return undefined;
    },

    getPathsToWatch() {
      return [
        path.join(adrsSource, '**/*.md'),
        path.join(specsSource, '**/*.md'),
      ];
    },
  };
};
