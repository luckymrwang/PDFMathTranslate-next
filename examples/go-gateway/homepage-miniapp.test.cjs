// node homepage-miniapp.test.cjs /path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root)
async function run() {
  let page, created = [], navigated = 0, previewed, relaunched = 0
  const app = { globalData: {} }, cache = {}
  const api = {
    getPayConfig: async () => ({ enabled: true, trial_enabled: true, trial_available: true, trial_pages: 3, trial_engine: 'Bing' }),
    createPreparedOrder: async (token, data) => {
      created.push({ token, data })
      return { order: { id: 'trial-1', engine: 'Bing', pages: 3 } }
    },
    listPayOrders: async () => ({ orders: [] }),
  }
  const sandbox = {
    require: name => {
      if (name.includes('storage-key')) return { storageKey: k => 'dev:' + k }
      if (name.endsWith('/api')) return api
      if (name.endsWith('/config')) return { demoMode: false }
      if (name.endsWith('/payment')) return require(path.join(root, 'utils/payment.js'))
      return {}
    },
    Page: p => { page = p },
    getApp: () => app,
    wx: {
      getStorageSync: k => cache[k], setStorageSync: (k, v) => { cache[k] = v },
      navigateTo: () => navigated++, showToast() {}, reLaunch: () => relaunched++,
      getImageInfo: ({ src, success }) => success({ path: src }),
      previewImage: options => { previewed = options },
    }, console,
  }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'pages/index/index.js'), 'utf8'), sandbox)
  page.data = JSON.parse(JSON.stringify(page.data))
  page.setData = (data, done) => { Object.assign(page.data, data); if (done) done() }
  await page.onPrimaryAction()
  assert.equal(created.length, 0, 'Empty homepage cannot create a trial')
  Object.assign(page.data, { filePath: 'file.pdf', fileName: 'file.pdf', filePreparation: 'ready', engine: 'GPT-6', mode: 'enhanced' })
  page.preparedFile = { path: 'file.pdf', file_token: 'prepared-token', page_count: 12, expires_at: Date.now() / 1000 + 1800 }
  await page.getPaymentConfig()
  await Promise.all([page.onPrimaryAction(), page.onPrimaryAction()])
  assert.equal(created.length, 1, 'Double taps cannot consume multiple trials')
  assert.equal(created[0].data.trial, true)
  assert.equal(created[0].data.engine, 'Bing', 'Trial uses standard engine independently of full-document choice')
  assert.equal(created[0].token, 'prepared-token')
  assert.equal(page.data.paymentSheet, false, 'No payment dialog for trial')
  assert.equal(navigated, 1)
  assert.equal(page.data.trialAvailable, false)
  assert.equal(app.globalData.translation.pageCount, 3)
  assert.equal(app.globalData.translation.originalPageCount, 12)
  assert.equal(app.globalData.translation.fullEngine, 'GPT-6')
  assert.ok(cache['dev:virtualpay_pending_jobs']['trial-1'])
  let quotes = 0
  page.openPaymentSheet = () => { quotes++ }
  await page.onPrimaryAction()
  assert.equal(quotes, 1, 'Returning users must see full-document payment quote')
  page.scrollToSamples();assert.equal(page.data.sampleAnchor, 'translation-samples')
  page.selectSample({ currentTarget: { dataset: { index: 1 } } })
  page.selectSampleVersion({ currentTarget: { dataset: { version: 'source' } } })
  assert.equal(page.data.sampleImage, '/assets/samples/manual-source.jpg')
  await page.previewSample();assert.equal(previewed.current, '/assets/samples/manual-source.jpg')
  vm.runInNewContext(fs.readFileSync(path.join(root, 'pages/preview/preview.js'), 'utf8'), { ...sandbox })
  page.setData = d => Object.assign(page.data, d)
  page.onLoad();assert.equal(page.data.trial, true)
  page.translateFull();assert.equal(relaunched, 1)
  assert.equal(app.globalData.translationReorder.id, 'trial-1')
  assert.equal(app.globalData.translationReorder.engine, 'GPT-6')
  console.log('PASS: direct three-page trial, duplicate taps, paid upgrade, sample controls and environment cache')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
