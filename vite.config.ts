import path from 'path'
// import react from '@vitejs/plugin-react'
// import svgr from 'vite-plugin-svgr'
import react from '@vitejs/plugin-react-swc' // use SWC plugin instead
import copy from 'rollup-plugin-copy'
import { defineConfig } from 'vite'
import { createHtmlPlugin } from 'vite-plugin-html'
import tsconfigPaths from 'vite-tsconfig-paths'

// Define file extensions
const fileExtensions = ['jpg', 'jpeg', 'png', 'gif', 'eot', 'otf', 'svg', 'ttf', 'woff', 'woff2']

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [
    react(),
    tsconfigPaths(),
    // copy({
    //   targets: [
    //     { src: 'src/assets/*', dest: 'www/dist/assets' },
    //     { src: 'src/notes/*', dest: 'www/dist/notes' },
    //     { src: 'src/assets/files/*', dest: 'www/dist' },
    //   ],
    //   hook: 'writeBundle',
    // }),
    createHtmlPlugin({
      pages: [
        {
          filename: 'index.html',
          template: 'index.html',
          entry: 'src/index.tsx',
          injectOptions: {
            data: {
              title: 'cursor',
            },
            // Add tags or ejsOptions if necessary
          },
        },
      ],
      minify: false,
    }),
  ],
  resolve: {
    alias: {
      // Define any aliases if necessary
    },
    extensions: fileExtensions.map((ext) => `.${ext}`).concat(['.js', '.jsx', '.ts', '.tsx']),
  },
  esbuild: {
    loader: 'tsx',
    target: 'esnext',
  },
  build: {
    outDir: 'www/dist',
    sourcemap: true, // Enable source maps
    rollupOptions: {
      input: {
        main: path.resolve(__dirname, 'src/index.tsx'), // Specifically set to your login entry point
      },
      output: {
        entryFileNames: '[name].bundle.js',
        assetFileNames: (assetInfo) => {
          // Route CSS to the app-assets folder, and other assets as needed
          if (assetInfo.name && assetInfo.name.endsWith('.css')) {
            return '[name][extname]'
          }
          return 'assets/[name].[hash][extname]'
        },
      },
    },
    assetsInlineLimit: 4096,
    emptyOutDir: true,
    modulePreload: false,
  },

  server: {
    port: 5989,
    // proxy: {
    //   '/assets': {
    //     target: 'http://localhost:1323',
    //     changeOrigin: true,
    //     rewrite: (p) => p.replace(/^\/assets/, '/assets'),
    //     configure: (proxy, options) => {
    //       proxy.on('proxyReq', (proxyReq, req) => {
    //         const userAgent = req.headers['user-agent']
    //         if (userAgent) {
    //           proxyReq.setHeader('User-Agent', userAgent)
    //         }
    //       })
    //     },
    //   },
    // },
  },
})
