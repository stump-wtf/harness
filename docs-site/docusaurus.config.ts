import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// ============================================================
// CONFIGURE THESE VALUES FOR YOUR PROJECT
// ============================================================
const PROJECT_TITLE = 'Harness';
const PROJECT_TAGLINE = 'systemctl for your agents — guides, usage, architecture decisions & specifications';
// The public GitHub mirror, not the Gitea origin: this site is public, and the
// Gitea host is private, so a link to it is a dead link for most readers.
const GITHUB_URL = 'https://github.com/stump-wtf/harness';
// Host this build is served from. The same build ships to more than one host —
// only `url` differs, so CI supplies it via DOCS_URL. Default is GitHub Pages,
// the public canonical: Gitea Pages resolves to a private address and is only
// reachable on the LAN.
//
// `||`, not `??`: the shared stump.wtf/ci static-site workflow exports DOCS_URL
// as an empty string when its site_url input is unset, and an empty `url` fails
// the Docusaurus build.
const SITE_URL = process.env.DOCS_URL || 'https://stump-wtf.github.io';
// Path prefix. Both GitHub Pages (/<repo>/) and Garage-backed Gitea Pages
// (<owner>.pages.stump.rocks/<repo>/) serve under the repo name.
const BASE_URL = process.env.DOCS_BASE_URL || '/harness/';
// ============================================================

const config: Config = {
  title: PROJECT_TITLE,
  tagline: PROJECT_TAGLINE,
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
  },

  url: SITE_URL,
  baseUrl: BASE_URL,

  // 'throw' (the Docusaurus default) so a broken link or anchor fails the
  // build instead of passing as a console warning (#365).
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  markdown: {
    format: 'detect',
    mermaid: true,
    hooks: {
      // Moved out of the top-level `onBrokenMarkdownLinks`, which is deprecated
      // and printed a migration warning on every build (twice, once per SSG
      // pass). Same behaviour, current location.
      onBrokenMarkdownLinks: 'throw',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  // Retries a failed lazy chunk (the Mermaid bundle is the big one) before
  // giving up, then offers a reload instead of leaving a half-broken page.
  clientModules: ['./src/clientModules/chunkLoadRetry.ts'],

  plugins: [
    ['./plugins/sdd-content', {
      adrsDir: '../docs/adrs',
      specsDir: '../docs/openspec/specs',
      outputDir: '../docs-generated',
    }],
    // build/llms.txt and build/llms-full.txt for agents. Always the public
    // GitHub Pages URL, never SITE_URL: the Gitea Pages twin of this build is
    // LAN-only, and the plugin fails the build on a private host.
    ['./plugins/llms-txt', {
      publicUrl: 'https://stump-wtf.github.io/harness/',
      summary:
        'Harness is systemctl for your agents: a daemon that supervises agent CLIs ' +
        '(Claude Code, Crush, Codex) and other long-running processes, restarts them, ' +
        'runs them on a schedule or on an event, and lets you attach to any of them. ' +
        'It is the supervision layer of a three-tool stack with Switchboard (dispatch) ' +
        'and Cairn (evidence).',
      startHere: [
        'guides/harness-switchboard-cairn',
        'guides/onboarding',
        'guides/glossary',
        'guides/install',
        'guides/first-agent',
        'guides/push-events',
      ],
    }],
  ],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs-generated',
          sidebarPath: './sidebars.ts',
          routeBasePath: '/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: PROJECT_TITLE,
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'guidesSidebar',
          position: 'left',
          label: 'Guides',
        },
        {
          type: 'docSidebar',
          sidebarId: 'usageSidebar',
          position: 'left',
          label: 'Usage',
        },
        {
          type: 'docSidebar',
          sidebarId: 'decisionsSidebar',
          position: 'left',
          label: 'ADRs',
        },
        {
          type: 'docSidebar',
          sidebarId: 'specsSidebar',
          position: 'left',
          label: 'Specifications',
        },
        {
          href: GITHUB_URL,
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Documentation',
          items: [
            {
              label: 'Getting started',
              to: '/guides',
            },
            {
              label: 'Usage',
              to: '/usage',
            },
            {
              label: 'Architecture Decisions',
              to: '/decisions',
            },
            {
              label: 'Specifications',
              to: '/specs',
            },
          ],
        },
        {
          title: 'Project',
          items: [
            {
              label: 'GitHub',
              href: GITHUB_URL,
            },
            {
              label: 'Switchboard',
              href: 'https://switchboard.stump.wtf/docs/',
            },
            {
              label: 'Cairn',
              href: 'https://cairn.stump.wtf/docs/intro/',
            },
          ],
        },
      ],
      copyright: `Copyright ${new Date().getFullYear()}. Built with Docusaurus.`,
    },
    // Mermaid picks a base theme per color mode; the site colors come from
    // src/css/custom.css ("Mermaid diagrams"), which reads the same tokens as
    // the rest of the site and so flips with data-theme on its own. Role
    // colors are CSS classes (`A["x"]:::daemon`), never classDef — see
    // docs-site/DIAGRAMS.md.
    mermaid: {
      theme: {light: 'neutral', dark: 'dark'},
      options: {
        // Layout measures labels in this face, so it must match the CSS,
        // or a label is sized for one font and drawn in another and clips.
        fontFamily: "'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, Consolas, monospace",
        fontSize: 14,
        flowchart: {curve: 'basis', padding: 12, htmlLabels: true},
        sequence: {mirrorActors: false, showSequenceNumbers: false, messageAlign: 'center'},
      },
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      // Grammars prism-react-renderer does not bundle, for every fence
      // language docs/ uses. `sh` is an alias the bash grammar registers.
      // `go` and `json` ship in the default bundle and are listed anyway so
      // this list alone says what the docs rely on. `logql` has no Prism
      // grammar and renders as plain text.
      additionalLanguages: ['go', 'bash', 'toml', 'ini', 'json', 'diff', 'promql'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
