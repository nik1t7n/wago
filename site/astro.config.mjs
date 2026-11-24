import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';

const repo = 'https://github.com/nik1t7n/wago';

export default defineConfig({
  site: 'https://nik1t7n.github.io',
  base: '/wago',
  integrations: [
    starlight({
      // code frames share the identity's radius and type everywhere, landing page included
      expressiveCode: { styleOverrides: { borderRadius: '14px', codeFontFamily: "'JetBrains Mono Variable', ui-monospace, Menlo, monospace" } },
      title: 'WaGo',
      description: 'Agent-first WhatsApp API in a single 21 MB Go binary.',
      logo: { light: '../docs/assets/logo.svg', dark: '../docs/assets/logo-dark.svg', replacesTitle: false },
      favicon: '/favicon.svg',
      social: [{ icon: 'github', label: 'GitHub', href: repo }],
      editLink: { baseUrl: `${repo}/edit/main/` },
      customCss: ['./src/styles/theme.css'],
      components: { Hero: './src/components/Hero.astro' },
      lastUpdated: false,
      sidebar: [
        { label: 'Start', items: ['getting-started', 'for-agents', 'examples'] },
        { label: 'Guides', items: ['agent-layer', 'configuration', 'migration', 'security'] },
        { label: 'Reference', items: ['api', 'design', 'changelog'] },
      ],
      plugins: [
        starlightLlmsTxt({
          projectName: 'WaGo',
          description: 'Agent-first WhatsApp API server in a single 21 MB Go binary: one turn.ready event per turn, one call to reply; typing, takeover, outbox, voice-to-text and number safety built in.',
          details: 'Start with the "For AI agents" page: setup, the rules (sending is the owner\'s decision, test with -dev-fake first), the reply contract and troubleshooting.',
          promote: ['for-agents', 'getting-started', 'agent-layer', 'api'],
        }),
      ],
    }),
  ],
});
