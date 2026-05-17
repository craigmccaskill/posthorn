// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://posthorn.dev',
  integrations: [
    starlight({
      title: 'Posthorn',
      description:
        'The unified outbound mail layer for self-hosted projects. One gateway between your apps and your transactional mail provider — Postmark today; Resend, Mailgun, SES coming. Self-hosted, no mail server required.',
      logo: {
        src: './src/assets/logo.svg',
        replacesTitle: false,
      },
      favicon: '/favicon.svg',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/craigmccaskill/posthorn',
        },
      ],
      head: [
        {
          tag: 'meta',
          attrs: {
            property: 'og:image',
            content: 'https://posthorn.dev/og.png',
          },
        },
        {
          tag: 'meta',
          attrs: { name: 'twitter:card', content: 'summary_large_image' },
        },
      ],
      customCss: ['./src/styles/custom.css'],
      sidebar: [
        {
          label: 'Getting started',
          items: [
            { label: 'Introduction', slug: 'getting-started/introduction' },
            { label: 'Installation', slug: 'getting-started/installation' },
            { label: 'Quick start', slug: 'getting-started/quick-start' },
            { label: 'Core concepts', slug: 'getting-started/concepts' },
          ],
        },
        {
          label: 'Configuration',
          items: [
            { label: 'TOML reference', slug: 'configuration/reference' },
            { label: 'Endpoints', slug: 'configuration/endpoints' },
            { label: 'Environment variables', slug: 'configuration/environment-variables' },
            {
              label: 'Transports',
              collapsed: true,
              items: [
                { label: 'Overview', slug: 'configuration/transports' },
                { label: 'Postmark', slug: 'configuration/transports/postmark' },
                { label: 'Resend', slug: 'configuration/transports/resend' },
                { label: 'Mailgun', slug: 'configuration/transports/mailgun' },
                { label: 'AWS SES', slug: 'configuration/transports/ses' },
                { label: 'Outbound SMTP', slug: 'configuration/transports/smtp' },
              ],
            },
          ],
        },
        {
          label: 'Deployment',
          items: [
            { label: 'Docker (recommended)', slug: 'deployment/docker' },
            { label: 'Standalone binary', slug: 'deployment/binary' },
            { label: 'Reverse proxy', slug: 'deployment/reverse-proxy' },
          ],
        },
        {
          label: 'Features',
          items: [
            { label: 'Spam protection', slug: 'features/spam-protection' },
            { label: 'Rate limiting', slug: 'features/rate-limiting' },
            { label: 'Validation', slug: 'features/validation' },
            { label: 'Templating', slug: 'features/templating' },
            { label: 'Retry policy', slug: 'features/retry-policy' },
            { label: 'Structured logging', slug: 'features/logging' },
            { label: 'API mode', slug: 'features/api-mode' },
            { label: 'SMTP ingress', slug: 'features/smtp-ingress' },
          ],
        },
        {
          label: 'Recipes',
          items: [
            { label: 'Contact form on a static site', slug: 'recipes/contact-form' },
            { label: 'Newsletter signup notification', slug: 'recipes/newsletter-signup' },
            { label: 'Multi-form site', slug: 'recipes/multi-form-site' },
            { label: 'Monitoring alerts via webhook', slug: 'recipes/monitoring-alerts' },
            { label: 'Transactional email from a Cloudflare Worker', slug: 'recipes/cloudflare-worker' },
          ],
        },
        {
          label: 'Security',
          items: [
            { label: 'Threat model', slug: 'security/threat-model' },
            { label: 'Header injection', slug: 'security/header-injection' },
            { label: 'API keys', slug: 'security/api-keys' },
            { label: 'DNS (SPF, DKIM, DMARC)', slug: 'security/dns' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'HTTP API', slug: 'reference/http-api' },
            { label: 'Response codes', slug: 'reference/response-codes' },
            { label: 'Log format', slug: 'reference/log-format' },
            { label: 'CLI', slug: 'reference/cli' },
          ],
        },
        { label: 'Roadmap', slug: 'roadmap' },
        { label: 'FAQ', slug: 'faq' },
      ],
    }),
  ],
});
