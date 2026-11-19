<script>
  import { onMount } from 'svelte'
  import { api } from './api.js'

  let { onExpired } = $props()
  let accounts = $state([])
  let busy = $state(true)
  let error = $state('')

  async function load() {
    accounts = await api('/api/access')
  }

  async function update(account) {
    busy = true
    error = ''
    try {
      if (account) {
        const action = account.granted ? 'revoke' : 'grant'
        await api(`/api/access/${account.id}/${action}`, { method: 'POST' })
      }
      await load()
    } catch (err) {
      if (err.status === 401) onExpired()
      else error = err.message
    } finally {
      busy = false
    }
  }

  onMount(() => { void update() })
</script>

{#if error}<p class="notice danger" role="alert">{error}</p>{/if}
<section class="panel" aria-label="PhotoLib 账号访问权限">
  <div class="panel-heading">
    <h2>PhotoLib 账号</h2>
    <button class="button" disabled={busy} onclick={() => update()}>刷新</button>
  </div>
  {#if !busy && !accounts.length && !error}
    <p class="empty">成员使用 PhotoLib 登录 Deplyt 后会出现在这里。</p>
  {/if}
  {#each accounts as account (account.id)}
    <div class="row">
      <div>
        <strong>{account.displayName || account.username}</strong>
        <p class="muted">{account.username} · #{account.id} · {account.granted ? '已授权' : '未授权'}</p>
      </div>
      <button class="button" disabled={busy} onclick={() => update(account)}>{account.granted ? '撤销权限' : '授予权限'}</button>
    </div>
  {/each}
</section>
