import { defineConfig } from '@playwright/test'
import { existsSync } from 'node:fs'

const edge = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'
export default defineConfig({
  testDir: './tests', fullyParallel: false, workers: 1, timeout: 45000,
  expect: { timeout: 10000 }, reporter: [['list'], ['html', { outputFolder: '../artifacts/playwright-report', open: 'never' }]],
  outputDir: '../artifacts/test-results',
  use: { baseURL: 'http://127.0.0.1:8088', viewport: { width: 1440, height: 960 }, screenshot: 'only-on-failure', trace: 'retain-on-failure', launchOptions: existsSync(edge) ? { executablePath: edge } : {} },
})
