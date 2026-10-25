import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import vm from 'node:vm'

const require = createRequire(import.meta.url)
const ts = require('typescript')
const vue = require('vue')
const axios = require('axios')

function compile(source) {
  return ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
}

function loadUtility(path) {
  const exports = {}
  vm.runInNewContext(compile(readFileSync(new URL(`../src/utils/${path}.ts`, import.meta.url), 'utf8')), { exports, require })
  return exports
}

function session() {
  return {
    id: 'onb-test', version: 3, mode: 'create_application', status: 'in_progress', current_step: 'identity',
    draft: { identity: { project_id: 'project', project_name: '', project_key: '', name: 'Application', key: 'duplicate', owner_user_id: 'owner', description: '' }, ci_pipeline_id: '', cd_pipeline_id: '', params: [], template_name: '', change_approval_flow: false, approval_flow_id: '', save_app_sources: false, repo_url: '', branch: '' },
    refs: { project_id: '', application_id: '', ci_binding_id: '', cd_binding_id: '', template_id: '' },
    check: { status: 'unknown', issues: [] }, first_release_order_id: '',
  }
}

function conflict(message, code) {
  return new axios.AxiosError(message, 'ERR_BAD_REQUEST', undefined, undefined, { status: 409, data: { error: message, ...(code ? { code } : {}) } })
}

function wizard(api) {
  const exports = {}
  const source = readFileSync(new URL('../src/views/onboarding/ApplicationOnboardingView.vue', import.meta.url), 'utf8').match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
  const modules = {
    vue: { ...vue, watch() {}, onMounted() {}, onBeforeUnmount() {} },
    'vue-router': { useRoute: () => ({ params: { sessionId: 'onb-test' } }), useRouter: () => ({}), onBeforeRouteLeave() {}, onBeforeRouteUpdate() {} },
    'ant-design-vue': { message: { warning() {} }, Modal: {} },
    axios,
    '../../api/onboarding': api,
    '../../api/project': {}, '../../api/pipeline': {}, '../../api/user': {}, '../../api/release': {},
    '../../stores/auth': { useAuthStore: () => ({ hasPermission: () => true }) },
    '../../utils/onboarding': loadUtility('onboarding'),
    '../../utils/http-error': loadUtility('http-error'),
  }
  vm.runInNewContext(compile(`${source}\nexport { accept, apply, save, session, draft, conflicted, activeStep };`), {
    exports, require: name => {
      assert.ok(name in modules, `Unexpected module ${name}`)
      return modules[name]
    }, structuredClone, setTimeout, clearTimeout,
  })
  exports.accept(session(), true)
  exports.activeStep.value = 1
  return exports
}

for (const duplicate of ['application key already exists', 'project key already exists', 'param key already exists']) {
  test(`correcting ${duplicate} can save and retry without reloading`, async () => {
    let current = session(), attempts = 0
    const ui = wizard({
      async applyOnboardingStep(s) {
        assert.equal(s.version, current.version)
        attempts++
        current = { ...current, version: current.version + 2 }
        if (attempts === 1) throw conflict(duplicate)
        return { ...current, current_step: 'pipelines', refs: { ...current.refs, application_id: 'app-created' } }
      },
      async getOnboardingSession() { return structuredClone(current) },
      async saveOnboardingDraft(s, draft) {
        assert.equal(s.version, current.version)
        current = { ...current, version: current.version + 1, draft }
        return structuredClone(current)
      },
    })
    await ui.apply()
    assert.equal(ui.conflicted.value, false)
    assert.equal(ui.session.value.version, current.version)
    ui.draft.value.identity.key = 'unique'
    await ui.apply()
    assert.equal(attempts, 2)
    assert.equal(ui.activeStep.value, 2)
    assert.equal(ui.session.value.refs.application_id, 'app-created')
  })
}

for (const code of [undefined, 'onboarding_conflict']) {
  test(`a real session conflict (${code || 'legacy response'}) keeps stale edits from overwriting the server`, async () => {
    let reads = 0, saves = 0
    const ui = wizard({
      async applyOnboardingStep() { throw conflict('配置已变化或正在保存，请刷新后重试', code) },
      async getOnboardingSession() { reads++; return session() },
      async saveOnboardingDraft() { saves++; return session() },
    })
    await ui.apply()
    assert.equal(ui.conflicted.value, true)
    ui.draft.value.identity.key = 'local-edit'
    await assert.rejects(ui.save(), /请先重新读取/)
    assert.equal(reads, 0)
    assert.equal(saves, 0)
  })
}

test('overlapping draft saves wait for the latest version and keep edits made during saving', async () => {
  let current = session(), releaseSave
  const calls = []
  const ui = wizard({
    async saveOnboardingDraft(s, draft) {
      calls.push({ version: s.version, key: draft.identity.key })
      if (calls.length === 1) await new Promise(resolve => { releaseSave = resolve })
      else await Promise.resolve()
      if (s.version !== current.version) throw conflict('配置已变化或正在保存，请刷新后重试', 'onboarding_conflict')
      current = { ...current, version: current.version + 1, draft }
      return structuredClone(current)
    },
  })
  ui.draft.value.identity.key = 'first-edit'
  const first = ui.save()
  ui.draft.value.identity.key = 'latest-edit'
  const second = ui.save(), third = ui.save()
  releaseSave()
  const results = await Promise.allSettled([first, second, third])
  assert.deepEqual(results.map(result => result.status), ['fulfilled', 'fulfilled', 'fulfilled'])
  assert.deepEqual(calls, [{ version: 3, key: 'first-edit' }, { version: 4, key: 'latest-edit' }])
  assert.equal(current.draft.identity.key, 'latest-edit')
  assert.equal(ui.conflicted.value, false)
})
