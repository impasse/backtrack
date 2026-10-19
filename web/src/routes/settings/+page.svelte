<script lang="ts">
	import { userFacingError } from '#lib/messages';
	import { onMount } from 'svelte';
	import { api } from '#lib/api';
	import { connectDvrEvents, notifyStreamsChange } from '#lib/events';
	import type { Stream } from '#lib/types';

	type Storage = {
		root: string;
		database: string;
		usedBytes: number;
		sourceBytes: number;
		freeBytes: number;
		segmentCount: number;
		clipBytes: number;
		libraryBytes: number;
		temporaryClipBytes: number;
		tempBytes: number;
		libraryMaxSizeBytes: number;
		temporaryMaxSizeBytes: number;
		temporaryRetentionSeconds: number;
		reservedFreeSpaceBytes: number;
		cleanupIntervalSeconds: number;
	};
	type Retention = { maxAgeHours: number; maxSizeBytes: number; preservePinned: boolean };
	type Category = 'channels' | 'storage' | 'service';

	let category = $state<Category>('channels');
	let role = $state<'admin' | 'viewer' | ''>('');
	let streams = $state<Stream[]>([]);
	let active = $state<Record<string, boolean>>({});
	let health = $state('正在检查…');
	let storage = $state<Storage | undefined>();
	let policy = $state<Retention>({
		maxAgeHours: 336,
		maxSizeBytes: 500 * 1024 ** 3,
		preservePinned: true,
	});
	let channelName = $state('');
	let busy = $state(false);
	let error = $state('');
	let saved = $state(false);
	let reveal = $state<Stream | undefined>();
	let revealRotated = $state(false);
	let editingStreamId = $state('');
	let editingStreamName = $state('');

	async function loadStreams() {
		const result = await api<{ streams: Stream[] }>('/api/streams');
		streams = result.streams ?? [];
		const states = await Promise.all(
			streams.map(async (stream) => {
				try {
					return [
						stream.id,
						(await api<{ live: boolean }>(`/api/streams/${stream.id}/status`)).live,
					] as const;
				} catch {
					return [stream.id, false] as const;
				}
			}),
		);
		active = Object.fromEntries(states);
	}

	async function load() {
		error = '';
		try {
			const [healthData, storageData, policyData] = await Promise.all([
				api<{ status: string }>('/api/system/health'),
				api<Storage>('/api/storage'),
				api<Retention>('/api/retention'),
			]);
			health = healthData.status === 'ok' ? '运行正常' : '状态未知';
			storage = storageData;
			policy = policyData;
			await loadStreams();
		} catch (e) {
			error = userFacingError(e);
		}
	}

	async function createChannel() {
		busy = true;
		error = '';
		try {
			const result = await api<{ stream: Stream }>('/api/streams', {
				method: 'POST',
				body: JSON.stringify({ name: channelName }),
			});
			channelName = '';
			reveal = result.stream;
			revealRotated = false;
			await loadStreams();
			notifyStreamsChange();
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	function startRename(stream: Stream) {
		editingStreamId = stream.id;
		editingStreamName = stream.name;
		error = '';
	}

	async function renameChannel(stream: Stream) {
		const name = editingStreamName.trim();
		if (!name) {
			error = '频道名称不能为空。';
			return;
		}
		busy = true;
		error = '';
		try {
			const updated = await api<Stream>(`/api/streams/${stream.id}`, {
				method: 'PUT',
				body: JSON.stringify({ name }),
			});
			streams = streams.map((item) => (item.id === updated.id ? updated : item));
			editingStreamId = '';
			notifyStreamsChange();
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	async function deleteChannel(stream: Stream) {
		const confirmed = window.confirm(
			`确定删除频道“${stream.name}”吗？该频道的录像、收藏片段和剪辑文件都会永久删除，此操作无法撤销。`,
		);
		if (!confirmed) return;
		busy = true;
		error = '';
		try {
			await api(`/api/streams/${stream.id}`, { method: 'DELETE' });
			streams = streams.filter((item) => item.id !== stream.id);
			if (reveal?.id === stream.id) reveal = undefined;
			if (editingStreamId === stream.id) editingStreamId = '';
			notifyStreamsChange();
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	async function toggleChannel(stream: Stream) {
		busy = true;
		error = '';
		try {
			const updated = await api<Stream>(`/api/streams/${stream.id}/enabled`, {
				method: 'PUT',
				body: JSON.stringify({ enabled: !stream.enabled }),
			});
			streams = streams.map((item) => (item.id === updated.id ? updated : item));
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	async function showPublishKey(stream: Stream) {
		busy = true;
		error = '';
		try {
			const result = await api<{ publishKey: string; rotated: boolean }>(
				`/api/streams/${stream.id}/publish-key`,
			);
			reveal = { ...stream, publishKey: result.publishKey };
			revealRotated = result.rotated;
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	async function saveRetention() {
		saved = false;
		error = '';
		try {
			await api('/api/retention', { method: 'PUT', body: JSON.stringify(policy) });
			saved = true;
		} catch (e) {
			error = userFacingError(e);
		}
	}

	function formatBytes(value: number) {
		return new Intl.NumberFormat('zh-CN', {
			style: 'unit',
			unit: 'gigabyte',
			unitDisplay: 'short',
			maximumFractionDigits: 1,
		}).format(value / 1e9);
	}

	function quotaPercent(value: number, limit: number) {
		return limit > 0 ? Math.min(100, Math.round((value / limit) * 100)) : 0;
	}

	function quotaDescription(value: number, limit: number) {
		return limit > 0 ? `${formatBytes(value)} / ${formatBytes(limit)}（${quotaPercent(value, limit)}%）` : `${formatBytes(value)} · 不限额`;
	}

	function formatDuration(seconds: number) {
		if (seconds <= 0) return '未设置';
		if (seconds % 86400 === 0) return `${seconds / 86400} 天`;
		if (seconds % 3600 === 0) return `${seconds / 3600} 小时`;
		if (seconds % 60 === 0) return `${seconds / 60} 分钟`;
		return `${seconds} 秒`;
	}

	onMount(() => {
		const requested = new URLSearchParams(window.location.search).get('section');
		if (requested === 'channels' || requested === 'storage' || requested === 'service')
			category = requested;
		connectDvrEvents(
			{
				'stream.created': () => void loadStreams(),
				'stream.updated': () => void loadStreams(),
				'storage.warning': () => void load(),
				'segment.committed': () => void load(),
				'stream.started': () => void loadStreams(),
				'stream.stopped': () => void loadStreams(),
				'stream.deleted': () => {
					void loadStreams();
					notifyStreamsChange();
				},
			},
			(auth) => {
				role = auth.role ?? '';
				if (auth.authenticated) void load();
				else {
					health = '需要连接';
					storage = undefined;
					streams = [];
					active = {};
					error = '';
				}
			},
		);
	});
</script>

<section class="page-head">
	<div>
		<div class="eyebrow">偏好与管理</div>
		<h1>设置</h1>
		<p>管理频道、录像保留规则和媒体服务信息。</p>
	</div>
	<button class="button secondary" onclick={load}>刷新设置</button>
</section>

<div class="settings-layout">
	<nav class="category-nav" aria-label="设置分类">
		<button
			class:chosen={category === 'channels'}
			aria-current={category === 'channels' ? 'page' : undefined}
			onclick={() => (category = 'channels')}
		>
			<span class="category-icon">◉</span>
			<span>
				<strong>频道管理</strong>
				<small>创建频道和控制推流接入</small>
			</span>
			<span class="category-count">{streams.length}</span>
		</button>
		<button
			class:chosen={category === 'storage'}
			aria-current={category === 'storage' ? 'page' : undefined}
			onclick={() => (category = 'storage')}
		>
			<span class="category-icon">▤</span>
			<span>
				<strong>存储与保留</strong>
				<small>容量概览和自动清理</small>
			</span>
		</button>
		<button
			class:chosen={category === 'service'}
			aria-current={category === 'service' ? 'page' : undefined}
			onclick={() => (category = 'service')}
		>
			<span class="category-icon">⌘</span>
			<span>
				<strong>服务信息</strong>
				<small>运行状态和存储位置</small>
			</span>
		</button>
	</nav>
	<div class="settings-content">
		{#if error}<div class="error">{error}</div>{/if}

		{#if category === 'channels'}
			<section class="section-heading">
				<div>
					<div class="eyebrow">频道设置</div>
					<h2>频道管理</h2>
					<p>创建频道、查看推流码，并控制频道是否接受新推流。</p>
				</div>
			</section>
			{#if role === 'admin'}
				<section class="panel create-panel">
					<div class="create-copy">
						<div class="channel-icon">＋</div>
						<div>
							<h3>创建频道</h3>
							<p>每个频道都有独立推流码，仅管理员可查看。</p>
						</div>
					</div>
					<form
						onsubmit={(event) => {
							event.preventDefault();
							void createChannel();
						}}
					>
						<input
							aria-label="频道名称"
							bind:value={channelName}
							placeholder="输入频道名称"
							maxlength="80"
						/>
						<button class="button" disabled={busy || !channelName.trim()}>
							{busy ? '创建中…' : '创建频道'}
						</button>
					</form>
				</section>
			{:else}
				<div class="readonly-note">当前为只读访问，频道管理仅管理员可操作。</div>
			{/if}
			{#if streams.length}
				<div class="channel-list">
					{#each streams as stream}
						<article class="panel channel-card">
							<div class="channel-main">
								<div class="channel-icon">◉</div>
								<div class="channel-copy">
									<div class="channel-title">
										{#if editingStreamId === stream.id}
											<form
												class="rename-form"
												onsubmit={(event) => {
													event.preventDefault();
													void renameChannel(stream);
												}}
											>
												<input
													aria-label="新频道名称"
													bind:value={editingStreamName}
													maxlength="80"
													required
												/>
												<button
													class="button secondary"
													disabled={busy || !editingStreamName.trim()}
												>
													保存
												</button>
												<button
													class="button secondary"
													type="button"
													disabled={busy}
													onclick={() => (editingStreamId = '')}
												>
													取消
												</button>
											</form>
										{:else}
											<h3>{stream.name}</h3>
										{/if}
										<span class:online={active[stream.id]} class="live-status">
											<i></i>
											{active[stream.id] ? '直播中' : '未开播'}
										</span>
									</div>
									<p>创建于 {new Date(stream.createdAt).toLocaleString()}</p>
								</div>
							</div>
							<div class="channel-actions">
								<span class:enabled={stream.enabled} class="ingest-state">
									<i></i>
									推流接入{stream.enabled ? '已启用' : '已停用'}
								</span>
								<a
									class="button secondary"
									href={`/live?streamId=${encodeURIComponent(stream.id)}`}
								>
									打开频道
								</a>
								{#if role === 'admin'}<button
										class="button secondary"
										disabled={busy}
										onclick={() => void showPublishKey(stream)}
									>
										显示推流码
									</button>
									<button
										class="button secondary"
										disabled={busy}
										onclick={() => void toggleChannel(stream)}
									>
										{stream.enabled ? '停用接入' : '启用接入'}
									</button>{/if}
								{#if role === 'admin'}
									<button
										class="button secondary"
										disabled={busy}
										onclick={() => startRename(stream)}
									>
										重命名
									</button>
									<button
										class="button danger"
										disabled={busy || active[stream.id]}
										title={active[stream.id] ? '停止推流后才能删除' : '删除频道和关联录像'}
										onclick={() => void deleteChannel(stream)}
									>
										删除频道
									</button>
								{/if}
							</div>
						</article>
					{/each}
				</div>
			{:else}
				<div class="empty-state">
					<div class="channel-icon">◉</div>
					<h3>还没有频道</h3>
					<p>创建频道后，它会出现在频道列表中。</p>
				</div>
			{/if}
		{:else if category === 'storage'}
			<section class="section-heading">
				<div>
					<div class="eyebrow">容量与保留</div>
					<h2>存储与保留</h2>
					<p>查看录像和导出占用，并设置自动清理规则。</p>
				</div>
			</section>
			<div class="cards">
				<section class="panel metric">
					<small>原始录像</small>
					<strong>{storage ? formatBytes(storage.sourceBytes) : '—'}</strong>
					<span>{storage?.segmentCount ?? '—'} 个录像片段 · 容量上限 {formatBytes(policy.maxSizeBytes)}</span>
					<div class="quota-track" aria-label="原始录像容量使用比例">
						<div class:quota-warning={quotaPercent(storage?.sourceBytes ?? 0, policy.maxSizeBytes) >= 90} style={`width: ${quotaPercent(storage?.sourceBytes ?? 0, policy.maxSizeBytes)}%`}></div>
					</div>
					<small class="quota-caption">已使用 {quotaPercent(storage?.sourceBytes ?? 0, policy.maxSizeBytes)}%</small>
				</section>
				<section class="panel metric">
					<small>素材库</small>
					<strong>{storage ? formatBytes(storage.libraryBytes) : '—'}</strong>
					<span>{storage ? quotaDescription(storage.libraryBytes, storage.libraryMaxSizeBytes) : '—'}</span>
					{#if storage && storage.libraryMaxSizeBytes > 0}
						<div class="quota-track" aria-label="素材库容量使用比例">
							<div class:quota-warning={quotaPercent(storage.libraryBytes, storage.libraryMaxSizeBytes) >= 90} style={`width: ${quotaPercent(storage.libraryBytes, storage.libraryMaxSizeBytes)}%`}></div>
						</div>
					{/if}
					<small class="quota-caption">保存的导出文件不会按临时文件保留期限自动删除</small>
				</section>
				<section class="panel metric">
					<small>临时下载</small>
					<strong>{storage ? formatBytes(storage.temporaryClipBytes) : '—'}</strong>
					<span>{storage ? quotaDescription(storage.temporaryClipBytes, storage.temporaryMaxSizeBytes) : '—'}</span>
					{#if storage && storage.temporaryMaxSizeBytes > 0}
						<div class="quota-track" aria-label="临时下载容量使用比例">
							<div class:quota-warning={quotaPercent(storage.temporaryClipBytes, storage.temporaryMaxSizeBytes) >= 90} style={`width: ${quotaPercent(storage.temporaryClipBytes, storage.temporaryMaxSizeBytes)}%`}></div>
						</div>
					{/if}
					<small class="quota-caption">未保存下载保留 {formatDuration(storage?.temporaryRetentionSeconds ?? 0)}</small>
				</section>
				<section class="panel metric">
					<small>进行中的导出</small>
					<strong>{storage ? formatBytes(storage.tempBytes) : '—'}</strong>
					<span>导出任务处理中占用的临时空间，不计入录像保留容量</span>
				</section>
				<section class="panel metric">
					<small>Backtrack 文件占用</small>
					<strong>{storage ? formatBytes(storage.usedBytes) : '—'}</strong>
					<span>原始录像、素材库、临时下载和进行中的导出</span>
				</section>
				<section class="panel metric">
					<small>磁盘可用空间</small>
					<strong>{storage ? formatBytes(storage.freeBytes) : '—'}</strong>
					<span class:storage-warning={!!storage && storage.freeBytes < storage.reservedFreeSpaceBytes}>
						预留线 {storage ? formatBytes(storage.reservedFreeSpaceBytes) : '—'} ·
						{storage && storage.freeBytes < storage.reservedFreeSpaceBytes ? '低于预留空间' : '宿主机文件系统当前可用容量'}
					</span>
				</section>
			</div>
			<section class="panel settings-card">
				<div class="section-heading">
					<div>
						<div class="eyebrow">自动清理</div>
						<h2>自动清理策略</h2>
						<p>录像超过保留天数或原始录像容量达到上限时，系统会优先清理较早的录像。</p>
					</div>
				</div>
				<div class="fields">
					<label>
						最多保留天数
						<input
							type="number"
							min="1"
							disabled={role !== 'admin'}
							value={policy.maxAgeHours / 24}
							oninput={(event) => (policy.maxAgeHours = Number(event.currentTarget.value) * 24)}
						/>
					</label>
					<label>
						最大原始录像容量（GB）
						<input
							type="number"
							min="1"
							disabled={role !== 'admin'}
							value={policy.maxSizeBytes / 1e9}
							oninput={(event) => (policy.maxSizeBytes = Number(event.currentTarget.value) * 1e9)}
						/>
					</label>
					<label class="check">
						<input
							type="checkbox"
							disabled={role !== 'admin'}
							bind:checked={policy.preservePinned}
						/>
						保留已保护录像
					</label>
					{#if role === 'admin'}<button class="button" onclick={saveRetention}>保存策略</button>
						{#if saved}<span class="saved">策略已保存</span>{/if}{:else}<span class="saved">
							只读访问
						</span>{/if}
				</div>
				<p class="retention-note">
					后台每 {formatDuration(storage?.cleanupIntervalSeconds ?? 0)} 检查一次，保存后会在下一轮检查时应用。
					{#if policy.preservePinned}
						已保护录像会跳过自动清理，所以磁盘占用可能仍高于容量上限；要释放空间，可先取消保护。
					{:else}
						保护标记不会阻止自动清理，达到保留条件的录像仍可能被删除。
					{/if}
				</p>
			</section>
			<section class="panel settings-card">
				<div class="section-heading">
					<div>
						<div class="eyebrow">导出文件保留</div>
						<h2>素材库与临时下载</h2>
						<p>导出文件使用独立于原始录像的容量限制；这些参数由服务端配置管理。</p>
					</div>
				</div>
				<div class="retention-summary">
					<div><span>素材库容量上限</span><strong>{storage ? (storage.libraryMaxSizeBytes > 0 ? formatBytes(storage.libraryMaxSizeBytes) : '不限额') : '—'}</strong></div>
					<div><span>临时下载容量上限</span><strong>{storage ? (storage.temporaryMaxSizeBytes > 0 ? formatBytes(storage.temporaryMaxSizeBytes) : '不限额') : '—'}</strong></div>
					<div><span>临时下载保留期限</span><strong>{formatDuration(storage?.temporaryRetentionSeconds ?? 0)}</strong></div>
				</div>
				<p class="retention-note">临时下载过期后会自动删除；素材库文件不会按时间自动过期。容量超限时会优先清理较早且未保护的导出文件。</p>
			</section>
		{:else}
			<section class="section-heading">
				<div>
					<div class="eyebrow">运行状态</div>
					<h2>服务信息</h2>
					<p>查看媒体服务健康状态和存储根目录。</p>
				</div>
			</section>
			<div class="service-grid">
				<section class="panel service-card">
					<div class="eyebrow">媒体服务</div>
					<h3>服务状态</h3>
					<strong
						class:green={health === '运行正常'}
						class:waiting={health === '需要连接'}
						class="health"
					>
						● {health}
					</strong>
					<p>HTTP API · SQLite WAL</p>
				</section>
				<section class="panel service-card">
					<div class="eyebrow">本地存储</div>
					<h3>存储根目录</h3>
					<strong class="root-path">{storage?.root ?? '—'}</strong>
					<p>录像和导出文件保存的位置。</p>
					<button
						class="button secondary"
						onclick={() => window.open('/api/system/health', '_blank')}
					>
						查看服务状态 ↗
					</button>
				</section>
			</div>
		{/if}
	</div>
</div>

{#if reveal}
	<div class="modal-backdrop">
		<section class="modal" aria-labelledby="created-title">
			<div class="eyebrow">推流信息</div>
			<h2 id="created-title">{reveal.publishKey ? '频道推流信息' : '频道已创建'}</h2>
			<p>推流码仅管理员可查看。请妥善保管，避免分享给他人。</p>
			{#if revealRotated}<p class="error">
					此频道使用旧版密钥存储，原密钥无法恢复；已生成新推流码，旧码已失效。
				</p>{/if}
			<label>
				RTMP 服务器
				<input
					readonly
					value={`rtmp://${typeof window === 'undefined' ? 'localhost' : window.location.hostname}:1935/live`}
				/>
			</label>
			<label>
				OBS 推流码
				<input readonly value={`${reveal.id}?key=${reveal.publishKey ?? ''}`} />
			</label>
			<div class="modal-actions">
				<a
					class="button"
					href={`/live?streamId=${encodeURIComponent(reveal.id)}`}
					onclick={() => (reveal = undefined)}
				>
					打开频道
				</a>
				<button class="button secondary" onclick={() => (reveal = undefined)}>关闭</button>
			</div>
		</section>
	</div>
{/if}

<style>
	.page-head {
		display: flex;
		justify-content: space-between;
		align-items: flex-end;
		gap: 16px;
		margin-bottom: 20px;
	}
	.eyebrow {
		font-size: 10px;
		letter-spacing: 1.6px;
		color: #7663c9;
		font-weight: 750;
	}
	h1 {
		font-size: 30px;
		line-height: 1.2;
		letter-spacing: -0.7px;
		margin: 7px 0;
	}
	h2 {
		font-size: 17px;
		color: #263143;
		margin: 5px 0;
	}
	h3 {
		font-size: 14px;
		color: #263143;
		margin: 0;
	}
	p {
		color: #758194;
		font-size: 12px;
		margin: 0;
	}
	.button {
		border: 1px solid #6653c7;
		color: #fff;
		background: #6653c7;
		border-radius: 8px;
		padding: 9px 14px;
		font-weight: 650;
		font-size: 12px;
		text-decoration: none;
		cursor: pointer;
		white-space: nowrap;
	}
	.button.secondary {
		border-color: #dfe4eb;
		background: #fff;
		color: #4b5668;
	}
	.button.danger {
		border-color: #f1c9cd;
		background: #fff3f4;
		color: #b33443;
	}
	.button:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.category-nav {
		display: flex;
		flex-direction: column;
		gap: 8px;
		position: sticky;
		top: 18px;
	}
	.category-nav button {
		position: relative;
		display: flex;
		align-items: center;
		gap: 11px;
		min-width: 0;
		width: 100%;
		text-align: left;
		padding: 12px 13px;
		border: 1px solid #e3e8ef;
		border-radius: 11px;
		background: #fff;
		color: #344054;
		cursor: pointer;
		box-shadow: 0 2px 7px #26354a08;
	}
	.category-nav button:hover {
		border-color: #cfc7f3;
	}
	.category-nav button.chosen {
		border-color: #bdb1f0;
		background: #f8f6ff;
		box-shadow: 0 0 0 2px #7663c916;
	}
	.settings-layout {
		display: grid;
		grid-template-columns: 225px minmax(0, 1fr);
		align-items: start;
		gap: 22px;
	}
	.settings-content {
		min-width: 0;
	}
	.category-icon,
	.channel-icon {
		width: 38px;
		height: 38px;
		flex: none;
		display: grid;
		place-items: center;
		border-radius: 11px;
		background: #f0edff;
		color: #6955c9;
		font-size: 18px;
	}
	.category-nav button > span:nth-child(2) {
		display: grid;
		gap: 4px;
		min-width: 0;
	}
	.category-nav strong {
		font-size: 12px;
	}
	.category-nav small {
		overflow: hidden;
		color: #8791a1;
		font-size: 10px;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.category-count {
		margin-left: auto;
		color: #8b82bf;
		font-size: 11px;
	}
	.section-heading {
		display: flex;
		justify-content: space-between;
		align-items: flex-end;
		margin: 0 0 13px;
	}
	.section-heading h2 {
		margin: 5px 0;
	}
	.section-heading p {
		font-size: 11px;
	}
	.panel {
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 12px;
		box-shadow: 0 2px 7px #26354a08;
	}
	.create-panel {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 20px;
		padding: 17px 18px;
		margin-bottom: 14px;
	}
	.create-copy {
		display: flex;
		align-items: center;
		gap: 12px;
	}
	.create-copy p {
		font-size: 11px;
		margin-top: 5px;
	}
	.create-panel form {
		display: flex;
		gap: 8px;
		width: min(430px, 55%);
	}
	input {
		min-width: 0;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
	}
	.create-panel form input {
		flex: 1;
	}
	.channel-list {
		display: grid;
		gap: 10px;
	}
	.channel-card {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 15px;
		padding: 14px 17px;
	}
	.channel-main {
		display: flex;
		align-items: center;
		gap: 12px;
		min-width: 0;
	}
	.channel-copy {
		min-width: 0;
	}
	.channel-copy p {
		font-size: 10px;
		margin-top: 5px;
	}
	.channel-title {
		display: flex;
		align-items: center;
		gap: 9px;
		flex-wrap: wrap;
	}
	.rename-form {
		display: flex;
		align-items: center;
		gap: 6px;
		flex-wrap: wrap;
	}
	.rename-form input {
		width: min(260px, 45vw);
	}
	.live-status,
	.ingest-state {
		display: inline-flex;
		align-items: center;
		gap: 6px;
		color: #8993a2;
		font-size: 10px;
	}
	.live-status i,
	.ingest-state i {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: #aab3c0;
	}
	.live-status.online {
		color: #16835f;
	}
	.live-status.online i,
	.ingest-state.enabled i {
		background: #35bd8c;
	}
	.channel-actions {
		display: flex;
		align-items: center;
		gap: 8px;
		flex: none;
	}
	.ingest-state {
		margin-right: 3px;
	}
	.readonly-note {
		padding: 11px 13px;
		margin-bottom: 13px;
		border: 1px solid #e9e3ce;
		border-radius: 9px;
		background: #fffaf0;
		color: #806c42;
		font-size: 11px;
	}
	.empty-state {
		display: grid;
		justify-items: center;
		text-align: center;
		gap: 8px;
		padding: 35px 20px;
		border: 1px dashed #dce2ea;
		border-radius: 12px;
		background: #fff;
	}
	.empty-state p {
		font-size: 11px;
	}
	.cards {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 10px;
		margin-bottom: 13px;
	}
	.metric {
		display: grid;
		gap: 6px;
		padding: 15px;
		min-width: 0;
	}
	.metric small {
		color: #8490a2;
		font-size: 10px;
	}
	.metric strong {
		color: #263143;
		font-size: 21px;
		line-height: 1.2;
	}
	.metric span {
		color: #758194;
		font-size: 10px;
	}
	.quota-track {
		height: 6px;
		overflow: hidden;
		border-radius: 999px;
		background: #e8edf3;
	}
	.quota-track > div {
		height: 100%;
		border-radius: inherit;
		background: #5c8ee6;
		transition: width 180ms ease;
	}
	.quota-track > div.quota-warning {
		background: #e7a13c;
	}
	.metric small.quota-caption {
		color: #8490a2;
		font-size: 9px;
	}
	.metric .storage-warning {
		color: #c2781b;
	}
	.settings-card {
		padding: 18px;
		margin-bottom: 13px;
	}
	.settings-card .section-heading {
		margin-bottom: 17px;
	}
	.retention-note {
		margin: 14px 0 0;
		padding: 10px 12px;
		border-radius: 9px;
		background: #f4f6f9;
		color: #687487;
		font-size: 10px;
		line-height: 1.6;
	}
	.retention-summary {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 10px;
	}
	.retention-summary > div {
		display: grid;
		gap: 5px;
		padding: 13px;
		border-radius: 10px;
		background: #f7f8fa;
	}
	.retention-summary span {
		color: #758194;
		font-size: 10px;
	}
	.retention-summary strong {
		color: #263143;
		font-size: 14px;
	}
	.fields {
		display: flex;
		align-items: flex-end;
		gap: 13px;
		flex-wrap: wrap;
	}
	.fields > label {
		display: grid;
		gap: 6px;
		color: #657185;
		font-size: 10px;
	}
	.fields input[type='number'] {
		width: 180px;
		min-height: 38px;
	}
	.fields .check {
		display: flex;
		align-items: center;
		gap: 7px;
		padding-bottom: 10px;
	}
	.saved {
		color: #16835f;
		font-size: 11px;
	}
	.service-grid {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 12px;
	}
	.service-card {
		display: grid;
		align-content: start;
		justify-items: start;
		gap: 9px;
		padding: 19px;
	}
	.service-card .eyebrow {
		font-size: 9px;
	}
	.service-card h3 {
		margin: 0;
	}
	.service-card p {
		font-size: 11px;
	}
	.health {
		font-size: 17px;
		color: #697589;
	}
	.health.green {
		color: #16835f;
	}
	.health.waiting {
		color: #a17b25;
	}
	.root-path {
		color: #344054;
		font-size: 13px;
		overflow-wrap: anywhere;
	}
	.service-card .button {
		margin-top: 4px;
	}
	.error {
		border: 1px solid #f4d6da;
		background: #fff2f3;
		color: #a63242;
		padding: 11px 14px;
		border-radius: 9px;
		margin-bottom: 14px;
		font-size: 12px;
	}
	.modal-backdrop {
		position: fixed;
		z-index: 10;
		inset: 0;
		background: #141826aa;
		display: grid;
		place-items: center;
		padding: 20px;
	}
	.modal {
		width: min(440px, 100%);
		border: 1px solid #e3e8ef;
		background: #fff;
		color: #202938;
		border-radius: 14px;
		padding: 25px;
		box-shadow: 0 20px 80px #0003;
	}
	.modal h2 {
		font-size: 20px;
		margin: 8px 0;
	}
	.modal > p {
		margin: 0 0 18px;
	}
	.modal label {
		display: grid;
		gap: 6px;
		color: #667286;
		font-size: 11px;
		margin-bottom: 12px;
	}
	.modal input {
		width: 100%;
	}
	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: 9px;
		margin-top: 18px;
	}
	@media (max-width: 900px) {
		.cards {
			grid-template-columns: repeat(2, minmax(0, 1fr));
		}
		.channel-card {
			align-items: flex-start;
			flex-direction: column;
		}
		.channel-actions {
			width: 100%;
			flex-wrap: wrap;
		}
	}
	@media (max-width: 650px) {
		.page-head {
			align-items: flex-start;
			flex-direction: column;
		}
		.settings-layout {
			grid-template-columns: minmax(0, 1fr);
			gap: 14px;
		}
		.category-nav {
			position: static;
			flex-direction: row;
			overflow-x: auto;
			gap: 7px;
			padding: 2px 2px 5px;
		}
		.category-nav button {
			flex: 0 0 190px;
			padding: 10px 12px;
		}
		.category-nav small {
			white-space: normal;
		}
		.create-panel {
			align-items: stretch;
			flex-direction: column;
		}
		.create-panel form {
			width: 100%;
		}
		.create-copy {
			align-items: flex-start;
		}
		.channel-actions {
			align-items: flex-start;
		}
		.cards,
		.service-grid {
			grid-template-columns: 1fr;
		}
		.retention-summary {
			grid-template-columns: 1fr;
		}
		.fields {
			align-items: stretch;
			flex-direction: column;
		}
		.fields label,
		.fields input[type='number'] {
			width: 100%;
		}
		.fields .check {
			align-self: flex-start;
		}
	}
</style>
