import { defineConfig } from 'vitepress'
import { readFileSync } from 'fs'
import { resolve } from 'path'
import { fileURLToPath } from 'url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))

// Read the version stamp written by `make set-version` (git describe output).
// Falls back to 'dev' when running outside a tagged release tree.
const versionFile = resolve(__dirname, '../../apiframework/version.txt')
let runtimeVersion = 'dev'
try {
    runtimeVersion = readFileSync(versionFile, 'utf-8').trim()
} catch {
    // not a release build — fine in local dev
}


export default defineConfig({
    title: 'Contenox Docs',
    description: 'Documentation for Contenox Vibe — AI workflows at your fingertips.',
    base: '/docs/',

    head: [
        ['link', { rel: 'icon', href: '/docs/favicon.png', type: 'image/png' }],
        ['meta', { name: 'theme-color', content: '#980000' }],
        ['meta', { property: 'og:type', content: 'website' }],
        ['meta', { property: 'og:site_name', content: 'Contenox Docs' }],
    ],

    themeConfig: {
        logo: { light: '/logo.png', dark: '/logo.png', alt: 'Contenox' },
        siteTitle: 'Contenox',

        nav: [
            { text: 'Guide', link: '/guide/introduction' },
            { text: 'Chains', link: '/chains/' },
            { text: 'Hooks', link: '/hooks/' },
            { text: 'CLI Reference', link: '/reference/vibe-cli' },
        ],

        sidebar: {
            '/guide/': [
                {
                    text: 'Getting Started',
                    items: [
                        { text: 'Introduction', link: '/guide/introduction' },
                        { text: 'Quickstart', link: '/guide/quickstart' },
                        { text: 'Core Concepts', link: '/guide/concepts' },
                    ],
                },
            ],
            '/chains/': [
                {
                    text: 'Task Chains',
                    items: [
                        { text: 'Overview', link: '/chains/' },
                        { text: 'Handlers', link: '/chains/handlers' },
                        { text: 'Transitions & Branching', link: '/chains/transitions' },
                        { text: 'Annotated Examples', link: '/chains/examples' },
                    ],
                },
            ],
            '/hooks/': [
                {
                    text: 'Hooks',
                    items: [
                        { text: 'What are Hooks?', link: '/hooks/' },
                        { text: 'Remote Hooks', link: '/hooks/remote' },
                        { text: 'Local Hooks', link: '/hooks/local' },
                    ],
                },
            ],
            '/reference/': [
                {
                    text: 'Reference',
                    items: [
                        { text: 'vibe CLI', link: '/reference/vibe-cli' },
                        { text: 'Configuration', link: '/reference/config' },
                    ],
                },
            ],
        },

        socialLinks: [
            { icon: 'github', link: 'https://github.com/contenox/vibe' },
        ],

        footer: {
            message: 'Released under the Apache 2.0 License.',
            copyright: `Copyright © ${new Date().getFullYear()} Contenox · Vibe ${runtimeVersion}`,
        },

        search: {
            provider: 'local',
        },

    },

    vite: {
        build: {
            chunkSizeWarningLimit: 1024,
        },
    },
})
