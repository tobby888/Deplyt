<script>
  import { onMount } from 'svelte'
  import { api, date, githubLink, repository } from './api.js'

  let { user, onExpired } = $props()
  let records = $state([])
  let loading = $state(true)
  let error = $state('')
  let feedback = $state('')
  let busy = $state(false)
  let workflowURL = $state('')
  let target = $state(null)
  let dialog
  let current = $derived(records.find(record => record.status === 'succeeded'))
  let latest = $derived(records[0])
  let uncertain = $derived(latest && ['failed', 'interrupted'].includes(latest.status))
  const labels = { running: '进行中', succeeded: '成功', failed: '失败', interrupted: '中断' }
  const kinds = { deploy: 'CI 发布', oss: 'OSS 手动部署', rollback: '管理员回滚' }

  async function reload() {
    loading = true
    error = ''
    try {
      records = await api('/api/deployments')
    } catch (err) {
      if (err.status === 401) onExpired()
      else error = err.message
    } finally {
      loading = false
    }
  }

  onMount(() => { void reload() })

  async function deploy(source) {
    if (busy) return
    busy = true
    workflowURL = ''
    feedback = source === 'oss' ? '正在下载并校验 OSS 制品，等待生产服务完成状态检查…' : '正在触发 PhotoLib 工作流…'
    try {
      const result = await api('/api/deployments/' + source, { method: 'POST' })
      if (source === 'workflow') {
        workflowURL = githubLink(result.url)
        feedback = '已触发 PhotoLib 工作流。'
      } else {
        feedback = '部署完成，生产服务已通过状态检查。'
      }
    } catch (err) {
      if (err.status === 401) onExpired()
      feedback = err.message
    } finally {
      busy = false
      if (source === 'oss') await reload()
    }
  }

  function confirm(record) {
    target = record
    dialog.showModal()
  }

  async function rollback(event) {
    event.preventDefault()
    if (busy || !target) return
    const id = target.id
    dialog.close()
    busy = true
    feedback = '正在从 OSS 下载并验证目标版本，等待生产服务重启…'
    try {
      await api(`/api/deployments/${id}/rollback`, { method: 'POST' })
      feedback = '回滚完成，生产服务已通过状态检查。'
    } catch (err) {
      if (err.status === 401) onExpired()
      feedback = '回滚请求未完成。' + err.message
    } finally {
      busy = false
      await reload()
    }
  }
</script>

<section class="production">
  <div>
    <p class="environment"><span class="dot"></span>生产环境</p>
    <h2>{current ? '最后确认版本' : '暂无已确认版本'}</h2>
    {#if current}
      <p class="production-version">{current.name}</p>
    {:else}
      <p>首次发布完成后将显示版本信息。</p>
    {/if}
  </div>
  <div class="production-meta">
    <span>确认时间</span>
    <strong>{date(current?.finished)}</strong>
  </div>
</section>

{#if latest && latest.status !== 'succeeded'}
  <p class="notice" role="status">最近一次操作{labels[latest.status]}。{latest.message ?? ''}</p>
{/if}
{#if user.workflow || user.artifact}
  <section class="panel" aria-label="手动部署">
    <div class="panel-heading">
      <div>
        <h2>手动部署</h2>
        {#if user.workflow}<p class="muted">PhotoLib 的 master 工作流通过构建和测试后发布到生产环境。</p>{/if}
        {#if user.artifact}<p class="muted">OSS 制品 <code>{user.artifact}</code></p>{/if}
      </div>
      <div class="login-actions">
        {#if user.workflow}<button class="button primary" disabled={busy} onclick={() => deploy('workflow')}>触发部署工作流</button>{/if}
        {#if user.artifact}<button class="button" disabled={busy || latest?.status === 'running'} onclick={() => deploy('oss')}>部署 OSS 制品</button>{/if}
      </div>
    </div>
  </section>
{/if}
{#if feedback}
  <p class="notice" role="status">{feedback} {#if workflowURL}<a href={workflowURL} target="_blank" rel="noreferrer">查看 GitHub Actions ↗</a>{/if}</p>
{/if}
{#if error}<p class="notice danger" role="alert">{error}</p>{/if}

<section class="panel">
  <div class="panel-heading">
    <div><h2>发布历史</h2><p class="muted">时间均为北京时间</p></div>
    <button class="button" disabled={loading} onclick={reload}>刷新</button>
  </div>
  {#if loading && !records.length}
    <p class="empty" role="status">正在读取部署记录…</p>
  {:else if !records.length && !error}
    <p class="empty">暂无部署记录。</p>
  {/if}
  {#each records as record (record.id)}
    <div class="release row">
      <div class="release-info">
        <div class="row-title">
          <span class={'status ' + record.status}>{labels[record.status] ?? record.status}</span>
          <strong>{kinds[record.kind] ?? record.kind}</strong>
          <span class="muted">{date(record.started)}</span>
        </div>
        <p class="file-name">{record.originalName}</p>
        {#if record.message}<p class="muted">{record.message}</p>{/if}
        <div class="links">
          {#if record.commit}<a href={repository + '/commit/' + record.commit} target="_blank" rel="noreferrer"><code>{record.commit.slice(0, 12)}</code> ↗</a>{/if}
          {#if record.run}<a href={repository + '/actions/runs/' + record.run} target="_blank" rel="noreferrer">运行 #{record.run} ↗</a>{/if}
          <span>{record.operator.username || 'GitHub Actions'}</span>
        </div>
        <details>
          <summary>归档与校验详情</summary>
          <dl class="facts">
            <div><dt>SHA-256</dt><dd><code>{record.sha256}</code></dd></div>
            {#if record.object}<div><dt>OSS 对象</dt><dd><code>{record.object}</code></dd></div>{/if}
            <div><dt>归档状态</dt><dd>{record.archived ? '已归档' : record.object ? '归档未获确认' : '直接中转'}</dd></div>
            <div><dt>文件大小</dt><dd>{(record.size / 1048576).toFixed(1)} MiB</dd></div>
            <div><dt>完成时间</dt><dd>{date(record.finished)}</dd></div>
            {#if record.target}<div><dt>目标记录</dt><dd><code>{record.target}</code></dd></div>{/if}
          </dl>
        </details>
      </div>
      {#if user.admin && user.archive && record.archived && record.status === 'succeeded' && (uncertain || record.object !== current?.object)}
        <button class="button" disabled={busy || latest?.status === 'running'} onclick={() => confirm(record)}>回滚到此版本</button>
      {:else if record.id === current?.id}
        <span class="status succeeded">最后确认</span>
      {/if}
    </div>
  {/each}
</section>

<dialog bind:this={dialog} aria-labelledby="rollback-title">
  <form onsubmit={rollback}>
    <h2 id="rollback-title">确认回滚生产版本</h2>
    <p>生产服务将短暂进入维护模式，并切换到以下 OSS 归档。</p>
    <p><code>{target?.name}</code></p>
    <div class="dialog-actions">
      <button type="button" class="button" onclick={() => dialog.close()}>取消</button>
      <button type="submit" class="button destructive">确认回滚</button>
    </div>
  </form>
</dialog>
