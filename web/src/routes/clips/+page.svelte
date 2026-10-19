<script lang="ts">
	import { userFacingError, clipStatusLabel } from '#lib/messages';
	import { onMount } from 'svelte';
	import { api } from '#lib/api';
	import { connectDvrEvents } from '#lib/events';
	import type { ClipJob, Stream } from '#lib/types';
	let streams = $state<Stream[]>([]);
	let jobs = $state<ClipJob[]>([]);
	let selected = $state('');
	let start = $state('');
	let end = $state('');
	let mode = $state('fast');
	let qualityPreset = $state('balanced');
	let resolution = $state('720p');
	let gapPolicy = $state('reject');
	let saveToLibrary = $state(false);
	let error = $state('');
	let busy = $state(false);
	let settingsDialogOpen = $state(false);
	let role = $state<'admin' | 'viewer' | ''>('');
	const streamName = (id: string) =>
		streams.find((stream) => stream.id === id)?.name ?? '频道已删除';
	const originalTimeHref = (job: ClipJob) =>
		`/live?${new URLSearchParams({
			streamId: job.request.streamId,
			tab: 'history',
			at: job.request.startTime,
		})}`;
	async function refresh() {
		error = '';
		try {
			const v = await api<{ jobs: ClipJob[] }>('/api/clips');
			jobs = v.jobs ?? [];
		} catch (e) {
			error = userFacingError(e);
		}
	}
	async function create() {
		busy = true;
		error = '';
		try {
			await api('/api/clips', {
				method: 'POST',
				body: JSON.stringify({
					streamId: selected,
					startTime: new Date(start).toISOString(),
					endTime: new Date(end).toISOString(),
					mode,
					qualityPreset,
					resolution,
					gapPolicy,
					saveToLibrary,
				}),
			});
			await refresh();
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}
	async function submitCreate() {
		settingsDialogOpen = false;
		await create();
	}
	async function remove(id: string) {
		error = '';
		try {
			await api(`/api/clips/${id}`, { method: 'DELETE' });
			await refresh();
		} catch (e) {
			error = userFacingError(e, '无法删除片段，请稍后重试。');
		}
	}
	async function pin(job: ClipJob) {
		error = '';
		try {
			await api(`/api/clips/${job.id}/pin`, {
				method: 'PUT',
				body: JSON.stringify({ pinned: !job.pinned }),
			});
			await refresh();
		} catch (e) {
			error = userFacingError(e, '无法更新收藏状态，请稍后重试。');
		}
	}
	const localInput = (value: string) => {
		const date = new Date(value);
		return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
	};
	async function initialize() {
		try {
			const v = await api<{ streams: Stream[] }>('/api/streams');
			streams = v.streams ?? [];
			const query = new URLSearchParams(window.location.search);
			selected = query.get('streamId') ?? streams[0]?.id ?? '';
			const startTime = query.get('startTime');
			const endTime = query.get('endTime');
			if (startTime) start = localInput(startTime);
			if (endTime) end = localInput(endTime);
			await refresh();
		} catch (e) {
			error = userFacingError(e);
		}
	}
	onMount(() =>
		connectDvrEvents(
			{ 'clip.progress': () => void refresh(), 'clip.completed': () => void refresh() },
			(auth) => {
				role = auth.role ?? '';
				if (auth.authenticated) void initialize();
				else {
					streams = [];
					jobs = [];
					error = '';
				}
			},
		),
	);
</script>

<div class="head">
	<div>
		<div class="eyebrow">片段工具</div>
		<h1>片段导出</h1>
		<p>选择起止时间后提交导出任务；处理完成即可下载 MP4。</p>
	</div>
</div>
{#if role === 'admin' && streams.length === 0}<div class="empty-state">
		还没有频道，创建频道并开始推流后即可导出片段。
		<a href="/settings?section=channels">创建频道</a>
	</div>{/if}
{#if role === 'admin'}<section class="panel form">
		<label>
			频道
			<select bind:value={selected}>
				<option value="">选择频道</option>
				{#each streams as s}<option value={s.id}>{s.name}</option>{/each}
			</select>
		</label>
		<label>
			开始时间
			<input type="datetime-local" bind:value={start} />
		</label>
		<label>
			结束时间
			<input type="datetime-local" bind:value={end} />
		</label>
		<label>
			<input type="checkbox" bind:checked={saveToLibrary} />
			保存到片段库
		</label>
		<button class="button" disabled={busy || !selected || !start || !end} onclick={() => (settingsDialogOpen = true)}>
			{busy ? '提交中…' : '创建导出任务'}
		</button>
	</section>{/if}
{#if settingsDialogOpen}<div class="modal-backdrop">
		<div class="modal" role="dialog" aria-modal="true" aria-labelledby="clip-export-title" tabindex="-1">
			<div class="eyebrow">导出设置</div>
			<h2 id="clip-export-title">创建导出任务</h2>
			<p>快速模式不重新编码；精确模式可以选择输出分辨率和画质。</p>
			<label>
				导出模式
				<select
					bind:value={mode}
					onchange={() => {
						if (mode === 'fast' && gapPolicy === 'fill') gapPolicy = 'reject';
					}}
				>
					<option value="fast">快速（起止点可能贴近关键帧）</option>
					<option value="accurate">精确（按选定时间重新编码）</option>
				</select>
			</label>
			{#if mode === 'accurate'}<label>
				输出分辨率
				<select bind:value={resolution}>
					<option value="source">保持源分辨率</option>
					<option value="1080p">1080p</option>
					<option value="720p">720p（默认）</option>
					<option value="480p">480p</option>
				</select>
			</label><label>
				画质预设
				<select bind:value={qualityPreset}>
					<option value="quality">画质优先（CRF 18，medium）</option>
					<option value="balanced">均衡（CRF 23，veryfast）</option>
					<option value="compact">体积优先（CRF 27，veryfast）</option>
				</select>
			</label>{/if}
			<label>
				缺口策略
				<select bind:value={gapPolicy}>
					<option value="reject">遇到缺口时拒绝</option>
					<option value="skip">跳过缺口</option>
					<option value="fill" disabled={mode === 'fast'}>填充黑屏和静音（精确模式）</option>
				</select>
			</label>
			<div class="modal-actions">
				<button class="button secondary" onclick={() => (settingsDialogOpen = false)}>取消</button>
				<button class="button" disabled={busy} onclick={submitCreate}>
					{busy ? '提交中…' : '开始导出'}
				</button>
			</div>
		</div>
	</div>{/if}
{#if error}<div class="error">{error}</div>{/if}
<section class="panel jobs">
	<div class="title">
		<h2>导出任务</h2>
		<button class="button secondary" onclick={refresh}>刷新</button>
	</div>
	{#each jobs as job}<div class="job">
			<div class="job-mark">✂</div>
			<div class="job-main">
				<strong>{streamName(job.request.streamId)}</strong>
				<small>
					{new Date(job.request.startTime).toLocaleString()} — {new Date(
						job.request.endTime,
					).toLocaleString()}
				</small>
			</div>
			<div class="progress">
				<div><i style={`width:${Math.max(0, Math.min(100, job.progress * 100))}%`}></i></div>
				<small>{clipStatusLabel(job.status)} · {Math.round(job.progress * 100)}%</small>
				{#if job.status === 'FAILED' && job.error}<small class="job-error">
						{userFacingError(job.error, '导出失败，请检查所选区间或服务配置。')}
					</small>{/if}
			</div>
			{#if job.status === 'COMPLETED'}<a
					class="button secondary"
					href={originalTimeHref(job)}
					title="跳转到该剪辑对应的直播历史时间点"
				>
					跳转原始时间
				</a>{#if role === 'admin'}<button
						class="button secondary"
						onclick={() => pin(job)}
					>
						{job.pinned ? '★ 已收藏' : '☆ 收藏'}
					</button>{/if}
				<a class="button" href={`/api/clips/${job.id}/download`} download={`clip-${job.id}.mp4`}>
					下载 MP4
				</a>{/if}{#if role === 'admin'}<button
					class="button secondary"
					onclick={() => remove(job.id)}
				>
					{job.status === 'COMPLETED' ? '删除片段' : '取消任务'}
				</button>{/if}
		</div>{:else}<div class="empty">
			还没有导出任务。选择频道和时间范围，创建第一个片段。
		</div>{/each}
</section>

<style>
	.empty-state {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 12px;
		margin: 0 0 15px;
		padding: 14px 16px;
		border: 1px solid #e4e8ef;
		border-radius: 10px;
		background: #fafbfd;
		color: #657185;
		font-size: 12px;
	}
	.empty-state a {
		color: #5845b7;
		font-weight: 650;
		text-decoration: none;
	}
	.empty-state a:hover {
		text-decoration: underline;
	}
	.head {
		margin-bottom: 23px;
	}
	.eyebrow {
		font-size: 10px;
		letter-spacing: 1.7px;
		color: #8a77ef;
		font-weight: 700;
	}
	h1 {
		font-size: 27px;
		margin: 6px 0 3px;
	}
	p {
		color: #8994a6;
		font-size: 12px;
		margin: 0;
	}
	.panel {
		background: #111620;
		border: 1px solid #252d3a;
		border-radius: 10px;
		padding: 17px;
		margin-bottom: 15px;
	}
	.form {
		display: grid;
		grid-template-columns: repeat(3, minmax(160px, 1fr));
		gap: 13px;
		align-items: end;
	}
	.form label {
		display: grid;
		gap: 6px;
		color: #8792a5;
		font-size: 10px;
	}
	.form input:not([type='checkbox']),
	.form select {
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		color: #273247;
		min-height: 40px;
	}
	.form input[type='checkbox'] {
		accent-color: #6653c7;
	}
	.button {
		background: #705bd1;
		border: 1px solid #826fe3;
		color: white;
		text-decoration: none;
		border-radius: 6px;
		padding: 8px 12px;
		font-size: 11px;
	}
	.modal-backdrop {
		position: fixed;
		z-index: 20;
		inset: 0;
		display: grid;
		place-items: center;
		padding: 20px;
		background: #090c12bb;
	}
	.modal {
		width: min(440px, 100%);
		padding: 24px;
		border: 1px solid #343e4e;
		border-radius: 13px;
		background: #171d28;
		color: #e8ebf1;
		box-shadow: 0 20px 70px #0008;
	}
	.modal h2 {
		margin: 7px 0;
		font-size: 19px;
	}
	.modal p {
		margin: 0 0 17px;
		color: #9ba5b5;
		font-size: 11px;
		line-height: 1.6;
	}
	.modal label {
		display: grid;
		gap: 6px;
		margin-bottom: 12px;
		color: #aab3c1;
		font-size: 10px;
	}
	.modal select {
		width: 100%;
		min-height: 38px;
		appearance: none;
		-webkit-appearance: none;
		padding: 8px 40px 8px 10px;
		border: 1px solid #3a4556;
		border-radius: 7px;
		background: #10151e;
		background-image: linear-gradient(45deg, transparent 50%, #e2e6ee 50%),
			linear-gradient(135deg, #e2e6ee 50%, transparent 50%);
		background-repeat: no-repeat;
		background-size: 5px 5px;
		background-position: calc(100% - 22px) 50%, calc(100% - 17px) 50%;
		color: #e2e6ee;
	}
	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: 8px;
		margin-top: 18px;
	}
	.button.secondary {
		background: #171d28;
		border-color: #303948;
		color: #c2cad7;
	}
	.title {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 11px;
	}
	h2 {
		font-size: 14px;
		margin: 0;
	}
	.job {
		display: flex;
		align-items: center;
		gap: 12px;
		border-top: 1px solid #252d3a;
		padding: 13px 0;
	}
	.job-mark {
		height: 31px;
		width: 31px;
		border-radius: 8px;
		background: #28213f;
		color: #ab98ff;
		display: grid;
		place-items: center;
	}
	.job-main {
		display: grid;
		gap: 3px;
		min-width: 210px;
	}
	.job-main strong {
		font-size: 11px;
	}
	.job-main small,
	.progress small {
		font-size: 9px;
		color: #7e899b;
	}
	.job-error {
		color: #f49aa3 !important;
	}
	.progress {
		width: 160px;
		margin-left: auto;
	}
	.progress > div {
		height: 5px;
		background: #252d3a;
		border-radius: 4px;
		overflow: hidden;
	}
	.progress i {
		display: block;
		height: 100%;
		background: #9b83fb;
	}
	.empty {
		text-align: center;
		color: #788498;
		padding: 38px;
		font-size: 12px;
	}
	.error {
		color: #f49aa3;
		font-size: 11px;
		margin: 10px 2px;
	}
	@media (max-width: 800px) {
		.form {
			grid-template-columns: 1fr 1fr;
		}
		.job {
			flex-wrap: wrap;
		}
		.progress {
			margin-left: 43px;
		}
		.job-main {
			min-width: unset;
		}
	}
	@media (max-width: 520px) {
		.form {
			grid-template-columns: 1fr;
		}
	}
</style>
