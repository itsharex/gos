<script setup lang="ts">
import { CopyOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons-vue'
import { message } from 'ant-design-vue'
import type { FormInstance, TableColumnsType } from 'ant-design-vue'
import type { Rule } from 'ant-design-vue/es/form'
import dayjs from 'dayjs'
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  createPersonalMCPToken,
  listPersonalMCPTokens,
  revokePersonalMCPToken,
} from '../../api/mcp-token'
import type { CreatedPersonalMCPToken, PersonalMCPToken } from '../../api/mcp-token'
import { extractHTTPErrorMessage } from '../../utils/http-error'

const tokens = ref<PersonalMCPToken[]>([])
const loading = ref(false)
const loadError = ref('')
const creating = ref(false)
const revokingID = ref('')
const createVisible = ref(false)
const secretVisible = ref(false)
const createdToken = ref<CreatedPersonalMCPToken | null>(null)
const formRef = ref<FormInstance>()
const form = reactive({ name: '', expires_in_days: 90 as number | null })
let tokenMutationVersion = 0
let tokenLoadVersion = 0

const columns: TableColumnsType<PersonalMCPToken> = [
  { title: '名称', dataIndex: 'name', key: 'name', width: 220 },
  { title: '创建时间', dataIndex: 'created_at', key: 'created_at', width: 190 },
  { title: '过期时间', dataIndex: 'expired_at', key: 'expired_at', width: 190 },
  { title: '状态', key: 'status', width: 130 },
  { title: '操作', key: 'actions', width: 120 },
]

const rules: Record<string, Rule[]> = {
  name: [{ required: true, whitespace: true, message: '请输入令牌名称', trigger: 'blur' }],
  expires_in_days: [
    {
      validator: async (_rule: Rule, value: number | null) => {
        if (typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 365) return
        throw new Error('有效期需为 1–365 天的整数')
      },
      trigger: 'change',
    },
  ],
}

// 示例只包含占位符；创建后的明文令牌不会插入配置示例或保存到列表。
const endpointExample = computed(() => JSON.stringify({
  mcpServers: {
    gos: {
      type: 'http',
      url: 'https://<GOS-host>/mcp',
      headers: { Authorization: 'Bearer <YOUR_MCP_TOKEN>' },
    },
  },
}, null, 2))

function formatTime(value: string | null | undefined) {
  if (!value) return '—'
  const date = dayjs(value)
  return date.isValid() ? date.format('YYYY-MM-DD HH:mm') : '—'
}

function tokenStatus(token: PersonalMCPToken) {
  if (token.revoked_at) return { label: '已吊销', color: 'default' }
  if (token.expired_at && dayjs(token.expired_at).isValid() && dayjs(token.expired_at).isBefore(dayjs())) {
    return { label: '已过期', color: 'orange' }
  }
  return { label: '有效', color: 'green' }
}

async function loadTokens() {
  loading.value = true
  loadError.value = ''
  const requestVersion = tokenMutationVersion
  const loadVersion = ++tokenLoadVersion
  try {
    const items = await listPersonalMCPTokens()
    if (!Array.isArray(items)) throw new Error('令牌列表格式错误')
    if (requestVersion === tokenMutationVersion && loadVersion === tokenLoadVersion) tokens.value = items
  } catch (error) {
    if (requestVersion !== tokenMutationVersion || loadVersion !== tokenLoadVersion) return
    tokens.value = []
    loadError.value = error instanceof Error && error.message === '令牌列表格式错误'
      ? error.message
      : extractHTTPErrorMessage(error, '令牌加载失败，请重试')
  } finally {
    if (loadVersion === tokenLoadVersion) loading.value = false
  }
}

function openCreate() {
  form.name = ''
  form.expires_in_days = 90
  formRef.value?.clearValidate()
  createVisible.value = true
}

function closeCreate() {
  if (creating.value) return
  createVisible.value = false
  form.name = ''
  form.expires_in_days = 90
}

async function submitCreate() {
  if (creating.value) return
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  const name = form.name.trim()
  const days = form.expires_in_days
  if (!name || typeof days !== 'number' || !Number.isInteger(days) || days < 1 || days > 365) return

  creating.value = true
  try {
    const result = await createPersonalMCPToken({ name, expires_in_days: days })
    if (!result?.token) {
      createVisible.value = false
      message.error('服务器未返回令牌明文，请检查令牌列表并重新创建')
      void loadTokens()
      return
    }
    // 仅在本次弹窗生命周期中持有明文；列表中只保留不含 token 的元数据。
    createdToken.value = result
    tokenMutationVersion += 1
    tokens.value = [{
      id: result.id,
      name: result.name,
      created_at: result.created_at,
      expired_at: result.expired_at,
      revoked_at: null,
    }, ...tokens.value.filter((item) => item.id !== result.id)]
    loadError.value = ''
    createVisible.value = false
    form.name = ''
    loading.value = false
    secretVisible.value = true
    message.success('令牌已创建')
  } catch (error) {
    message.error(extractHTTPErrorMessage(error, '创建令牌失败'))
  } finally {
    creating.value = false
  }
}

function closeSecret() {
  secretVisible.value = false
  createdToken.value = null
}

async function copyText(text: string, success: string) {
  try {
    if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable')
    await navigator.clipboard.writeText(text)
    message.success(success)
  } catch {
    message.warning('复制失败，请手动选中文本复制')
  }
}

async function revokeToken(token: PersonalMCPToken) {
  if (revokingID.value || token.revoked_at) return
  revokingID.value = token.id
  try {
    await revokePersonalMCPToken(token.id)
    tokenMutationVersion += 1
    tokens.value = tokens.value.map((item) => item.id === token.id
      ? { ...item, revoked_at: new Date().toISOString() }
      : item)
    loading.value = false
    message.success('令牌已吊销')
  } catch (error) {
    message.error(extractHTTPErrorMessage(error, '吊销令牌失败'))
  } finally {
    revokingID.value = ''
  }
}

onMounted(() => { void loadTokens() })
onBeforeUnmount(() => {
  // 离开页面时清除明文，刷新和重新进入页面都不能恢复它。
  createdToken.value = null
})
</script>

<template>
  <div class="page-wrapper mcp-token-page">
    <header class="page-header-card mcp-header">
      <div class="page-header-copy">
        <h1 class="page-title">个人 MCP 令牌</h1>
        <p class="page-subtitle">为 MCP 客户端创建独立的访问令牌。请妥善保存，令牌明文仅在创建时显示一次。</p>
      </div>
      <a-button type="primary" @click="openCreate">
        <template #icon><PlusOutlined /></template>
        创建令牌
      </a-button>
    </header>

    <section aria-labelledby="token-list-title" class="mcp-section">
      <div class="mcp-section-heading">
        <div>
          <h2 id="token-list-title">我的令牌</h2>
          <p>过期或吊销的令牌不能继续使用。吊销操作立即生效。</p>
        </div>
        <a-button :loading="loading" @click="loadTokens">
          <template #icon><ReloadOutlined /></template>
          刷新
        </a-button>
      </div>
      <a-alert v-if="loadError" type="error" show-icon class="mcp-list-error" :message="loadError">
        <template #action><a-button size="small" @click="loadTokens">重试</a-button></template>
      </a-alert>
      <a-table
        v-if="!loadError"
        row-key="id"
        :columns="columns"
        :data-source="tokens"
        :loading="loading"
        :pagination="{ pageSize: 10, hideOnSinglePage: true }"
        :scroll="{ x: 780 }"
      >
        <template #emptyText>
          <a-empty description="暂无 MCP 令牌">
            <a-button type="primary" @click="openCreate">创建第一个令牌</a-button>
          </a-empty>
        </template>
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'created_at'">{{ formatTime(record.created_at) }}</template>
          <template v-else-if="column.key === 'expired_at'">{{ formatTime(record.expired_at) }}</template>
          <template v-else-if="column.key === 'status'">
            <a-tag :color="tokenStatus(record).color">{{ tokenStatus(record).label }}</a-tag>
          </template>
          <template v-else-if="column.key === 'actions'">
            <a-popconfirm
              v-if="!record.revoked_at"
              :title="`确定吊销「${record.name}」吗？`"
              description="使用此令牌的客户端将立即失去访问权限。"
              ok-text="吊销"
              cancel-text="取消"
              @confirm="revokeToken(record)"
            >
              <a-button type="link" danger :loading="revokingID === record.id">吊销</a-button>
            </a-popconfirm>
            <span v-else class="mcp-muted">—</span>
          </template>
        </template>
      </a-table>
    </section>

    <section aria-labelledby="mcp-config-title" class="mcp-section mcp-config">
      <div class="mcp-section-heading">
        <div>
          <h2 id="mcp-config-title">客户端连接示例</h2>
          <p>在支持 HTTP MCP 的客户端中填写服务地址与授权请求头。</p>
        </div>
        <a-button @click="copyText(endpointExample, '配置示例已复制')">
          <template #icon><CopyOutlined /></template>
          复制示例
        </a-button>
      </div>
      <pre class="mcp-code"><code>{{ endpointExample }}</code></pre>
      <p class="mcp-hint">请将地址替换为实际部署的 MCP 服务端点，并将 <code>&lt;YOUR_MCP_TOKEN&gt;</code> 替换为刚创建的令牌。不同部署的端点路径可能不同，请以服务配置为准。</p>
    </section>

    <a-modal
      :open="createVisible"
      title="创建个人 MCP 令牌"
      ok-text="创建"
      cancel-text="取消"
      :confirm-loading="creating"
      :ok-button-props="{ disabled: creating }"
      :mask-closable="!creating"
      :closable="!creating"
      :keyboard="!creating"
      :width="520"
      @ok="submitCreate"
      @cancel="closeCreate"
    >
      <a-form ref="formRef" :model="form" :rules="rules" layout="vertical" class="mcp-create-form">
        <a-form-item label="名称" name="name">
          <a-input v-model:value="form.name" maxlength="100" placeholder="例如：本地开发工具" autocomplete="off" />
        </a-form-item>
        <a-form-item label="有效期（天）" name="expires_in_days">
          <a-input-number v-model:value="form.expires_in_days" :min="1" :max="365" :precision="0" :step="1" class="mcp-duration" />
        </a-form-item>
        <p class="mcp-hint">默认 90 天，最长 365 天。到期后需要创建新令牌。</p>
      </a-form>
    </a-modal>

    <a-modal
      :open="secretVisible"
      title="请立即保存令牌"
      :width="560"
      :mask-closable="false"
      ok-text="已保存，关闭"
      :cancel-button-props="{ style: { display: 'none' } }"
      @ok="closeSecret"
      @cancel="closeSecret"
    >
      <a-alert type="warning" show-icon message="令牌明文仅显示这一次。关闭此窗口后无法再次查看。" class="mcp-secret-warning" />
      <label for="mcp-created-token" class="mcp-secret-label">{{ createdToken?.name }} 的访问令牌</label>
      <a-textarea id="mcp-created-token" :value="createdToken?.token" :rows="3" readonly class="mcp-secret-value" />
      <a-button type="primary" class="mcp-copy-secret" @click="createdToken && copyText(createdToken.token, '令牌已复制')">
        <template #icon><CopyOutlined /></template>
        复制令牌
      </a-button>
      <p class="mcp-hint">有效期至 {{ formatTime(createdToken?.expired_at) }}。请在受信任的客户端中使用，避免将令牌提交到代码仓库。</p>
    </a-modal>
  </div>
</template>

<style scoped>
.mcp-token-page { max-width: 1200px; margin: 0 auto; }
.mcp-header, .mcp-section-heading { display: flex; align-items: center; justify-content: space-between; gap: 20px; }
.mcp-header { flex-wrap: wrap; }
.mcp-section { min-width: 0; padding: 24px; border: 1px solid rgba(148, 163, 184, 0.16); border-radius: var(--radius-xl); background: rgba(255, 255, 255, 0.9); box-shadow: 0 14px 32px rgba(15, 23, 42, 0.04); }
.mcp-section-heading { margin-bottom: 20px; }
.mcp-section-heading h2 { margin: 0 0 6px; color: #0f172a; font-size: 18px; font-weight: 700; }
.mcp-section-heading p, .mcp-hint { margin: 0; color: #64748b; font-size: 13px; line-height: 1.7; }
.mcp-section-heading .ant-btn, .mcp-header .ant-btn { flex: 0 0 auto; }
.mcp-list-error { margin-bottom: 0; }
.mcp-code { overflow-x: auto; padding: 18px; margin: 0 0 14px; border-radius: 12px; background: #0f172a; color: #e2e8f0; font-size: 13px; line-height: 1.6; white-space: pre; }
.mcp-hint code { overflow-wrap: anywhere; }
.mcp-muted { color: #94a3b8; }
.mcp-create-form { padding-top: 12px; }
.mcp-create-form .mcp-hint { margin-bottom: 0; }
.mcp-duration { width: 100%; }
.mcp-secret-warning { margin-bottom: 20px; }
.mcp-secret-label { display: block; margin-bottom: 8px; font-weight: 600; }
.mcp-secret-value { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.mcp-copy-secret { margin: 12px 0; }
@media (max-width: 640px) {
  .mcp-section { padding: 16px; }
  .mcp-section-heading { align-items: flex-start; flex-wrap: wrap; }
  .mcp-section-heading .ant-btn { margin-left: auto; }
}
</style>
