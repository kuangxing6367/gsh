import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'gsh',
  titleTemplate: ':title · 终端式 AI Agent',
  description:
    'gsh —— 微内核 + 插件化的终端式 AI Agent：TUI、Web UI、CLI 三端对等，会话可 resume / fork / replay / 只读分享。',
  appearance: 'dark',
  lastUpdated: true,
  ignoreDeadLinks: true,
  themeConfig: {
    nav: [
      { text: '指南', link: '/guide/intro' },
      { text: 'GitHub', link: 'https://github.com/kuangxing6367/gsh' },
    ],
    sidebar: [
      {
        text: '指南',
        items: [
          { text: '介绍', link: '/guide/intro' },
          { text: '快速开始', link: '/guide/quickstart' },
          { text: 'CLI 用法', link: '/guide/cli' },
          { text: '终端 UI', link: '/guide/tui' },
          { text: '架构', link: '/guide/architecture' },
          { text: '配置', link: '/guide/config' },
          { text: '会话管理', link: '/guide/sessions' },
          { text: '开发', link: '/guide/dev' },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/kuangxing6367/gsh' },
    ],
    footer: {
      message: 'gsh · MIT License',
      copyright: '由 zc team 维护',
    },
    search: {
      provider: 'local',
    },
  },
})
