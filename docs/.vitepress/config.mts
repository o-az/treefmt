import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: "Treefmt",
  description: "one CLI to format your repo",
  themeConfig: {

    logo: '../assets/logo.svg',

    // https://vitepress.dev/reference/default-theme-config
    nav: [
      { text: 'Home', link: '/' },
      { text: 'Quick Start', link: '/quick-start' }
    ],

    sidebar: [
      { text: 'Quick Start', link: '/quick-start' },
      { text: 'Overview', link: '/overview' },
      { text: 'Usage', link: '/usage' },
      { text: 'Formatter Spec', link: '/formatter-spec' },
      { text: 'Contributing', link: '/contributing' },
      { text: 'FAQ', link: '/faq' },
    ],

    socialLinks: [
      { icon: 'github', link: 'https://github.com/vuejs/vitepress' }
    ]
  }
})
