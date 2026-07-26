import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// ============================================================
// CONFIGURE THESE VALUES FOR YOUR PROJECT
// ============================================================
const PROJECT_TITLE = 'Harness';
const PROJECT_TAGLINE = 'systemctl for your agents — architecture decisions & specifications';
const GITHUB_URL = 'https://gitea.stump.rocks/stump.wtf/harness';
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

  onBrokenLinks: 'warn',
  onBrokenMarkdownLinks: 'warn',

  markdown: {
    format: 'detect',
    mermaid: true,
  },

  themes: ['@docusaurus/theme-mermaid'],

  plugins: [
    ['./plugins/sdd-content', {
      adrsDir: '../docs/adrs',
      specsDir: '../docs/openspec/specs',
      outputDir: '../docs-generated',
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
          label: 'Gitea',
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
              label: 'Gitea',
              href: GITHUB_URL,
            },
          ],
        },
      ],
      copyright: `Copyright ${new Date().getFullYear()}. Built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['go', 'bash'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
