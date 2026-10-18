<script lang="ts">
	import { userFacingError } from '#lib/messages';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api } from '#lib/api';
	import { connectDvrEvents } from '#lib/events';
	import type { ClipJob, Stream } from '#lib/types';
	let jobs = $state<ClipJob[]>([]);
	let streams = $state<Stream[]>([]);
	let role = $state<'admin' | 'viewer' | ''>('');
	let error = $state('');
	let busy = $state(false);
	let playingClipId = $state('');
	let playerDialog: HTMLDialogElement;
	let favorites = $derived(
		jobs.filter(
			(job) =>
				job.status === 'COMPLETED' &&
				job.pinned &&
				(!page.url.searchParams.get('streamId') ||
					job.request.streamId === page.url.searchParams.get('streamId')),
		),
	);
	let playingJob = $derived(favorites.find((job) => job.id === playingClipId));
	$effect(() => {
		if (!playerDialog) return;
		if (playingClipId && !playerDialog.open) playerDialog.showModal();
		else if (!playingClipId && playerDialog.open) playerDialog.close();
	});
	async function load() {
		error = '';
		try {
			const [clips, channels] = await Promise.all([
				api<{ jobs: ClipJob[] }>('/api/clips'),
				api<{ streams: Stream[] }>('/api/streams'),
			]);
			jobs = clips.jobs ?? [];
			streams = channels.streams ?? [];
		} catch (e) {
			error = userFacingError(e);
		}
	}
	function channelName(streamId: string) {
		return streams.find((stream) => stream.id === streamId)?.name ?? '未知频道';
	}
	async function deleteFavorite(job: ClipJob) {
		const name = job.request.name || `片段 ${new Date(job.request.startTime).toLocaleString()}`;
		if (!window.confirm(`确定永久删除“${name}”吗？视频文件将被删除，无法恢复。`)) return;
		busy = true;
		error = '';
		try {
			await api(`/api/clips/${job.id}`, { method: 'DELETE' });
			if (playingClipId === job.id) playingClipId = '';
			await load();
		} catch (e) {
			error = userFacingError(e, '无法删除收藏视频，请稍后重试。');
		} finally {
			busy = false;
		}
	}
	function closePlayer() {
		playingClipId = '';
	}
	onMount(() =>
		connectDvrEvents(
			{ 'clip.completed': () => void load(), 'clip.progress': () => void load() },
			(auth) => {
				role = auth.role ?? '';
				if (auth.authenticated) void load();
				else {
					jobs = [];
					streams = [];
				}
			},
		),
	);
</script>

<section class="page-head">
	<div>
		<div class="eyebrow">珍藏片段</div>
		<h1>收藏片段</h1>
		<p>{page.url.searchParams.get('streamId') ? '当前频道收藏的直播片段，可随时回看和下载。' : '所有频道收藏的直播片段，可随时回看和下载。'}</p>
	</div>
	<button class="button secondary" onclick={load}>刷新</button>
</section>
{#if error}<div class="error">{error}</div>{/if}
{#if favorites.length}
	<div class="favorite-list">
		{#each favorites as job}<article class="favorite-card">
				<div class="favorite-icon">★</div>
				<div class="favorite-main">
					<div class="favorite-title">
						<strong>
							{job.request.name || `片段 ${new Date(job.request.startTime).toLocaleString()}`}
						</strong>
						<span>已收藏</span>
					</div>
					<small>
						{new Date(job.request.startTime).toLocaleString()} — {new Date(
							job.request.endTime,
						).toLocaleTimeString()} · 频道 {channelName(job.request.streamId)} · 收藏于 {new Date(
							job.createdAt,
						).toLocaleString()}
					</small>
					<div class="clip-track"><i></i></div>
				</div>
				<div class="actions">
					<button
						class="button secondary"
						type="button"
						aria-label="播放片段"
						onclick={() => (playingClipId = job.id)}
					>
						播放
					</button>
					<a class="button" href={`/api/clips/${job.id}/download`} download={`clip-${job.id}.mp4`}>
						下载 MP4
					</a>
					{#if role === 'admin'}<button
							class="button secondary"
							type="button"
							disabled={busy}
							onclick={() => deleteFavorite(job)}
						>
							删除文件
						</button>{/if}
				</div>
			</article>{/each}
	</div>
{:else}
	<div class="empty-state">
		<div class="empty-icon">☆</div>
		<h2>还没有收藏片段</h2>
		<p>打开频道的直播历史，选好片段区间后点击“收藏”。</p>
		<a class="button" href="/channels">浏览频道</a>
	</div>
{/if}

<dialog
	class="player-dialog"
	bind:this={playerDialog}
	aria-labelledby="favorite-player-title"
	onclose={() => (playingClipId = '')}
	onclick={(event) => {
		if (event.target === playerDialog) closePlayer();
	}}
>
	{#if playingJob}
		<header class="player-dialog-head">
			<div>
				<h2 id="favorite-player-title">
					{playingJob.request.name || `片段 ${new Date(playingJob.request.startTime).toLocaleString()}`}
				</h2>
				<p>{channelName(playingJob.request.streamId)}</p>
			</div>
			<button class="button secondary" type="button" onclick={closePlayer}>关闭</button>
		</header>
		<div class="favorite-player">
			<!-- svelte-ignore a11y_media_has_caption -->
			<video controls autoplay playsinline preload="metadata" src={`/api/clips/${playingJob.id}/play`}>
				您的浏览器不支持视频播放。
			</video>
		</div>
	{/if}
</dialog>

<style>
	.page-head {
		display: flex;
		align-items: end;
		justify-content: space-between;
		margin-bottom: 24px;
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
	p {
		color: #758194;
		font-size: 13px;
		margin: 0;
	}
	.button {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		border: 1px solid #6653c7;
		color: #fff;
		background: #6653c7;
		border-radius: 8px;
		padding: 9px 14px;
		font-weight: 650;
		font-size: 12px;
		text-decoration: none;
		cursor: pointer;
	}
	.button.secondary {
		border-color: #dfe4eb;
		background: #fff;
		color: #4b5668;
	}
	.button:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.favorite-list {
		display: grid;
		gap: 10px;
	}
	.favorite-card {
		display: flex;
		align-items: center;
		gap: 15px;
		padding: 16px 18px;
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 12px;
		box-shadow: 0 2px 7px #26354a08;
	}
	.favorite-icon {
		width: 38px;
		height: 38px;
		flex: none;
		display: grid;
		place-items: center;
		border-radius: 11px;
		background: #fff7df;
		color: #d89b26;
		font-size: 17px;
	}
	.favorite-main {
		min-width: 0;
		flex: 1;
	}
	.favorite-player {
		width: 100%;
	}
	.player-dialog {
		width: min(80vw, 1100px);
		max-width: calc(100vw - 32px);
		max-height: 90vh;
		padding: 20px;
		border: 1px solid #e3e8ef;
		border-radius: 16px;
		background: #fff;
		box-shadow: 0 24px 80px #10182855;
	}
	.player-dialog::backdrop {
		background: #111827b8;
		backdrop-filter: blur(3px);
	}
	.player-dialog-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
		margin-bottom: 16px;
	}
	.player-dialog-head h2 {
		margin: 0;
		color: #273247;
		font-size: 16px;
	}
	.player-dialog-head p {
		margin-top: 5px;
	}
	.favorite-player video {
		display: block;
		width: 100%;
		max-height: 76vh;
		margin: 0 auto;
		background: #080a0d;
		border-radius: 10px;
	}
	.favorite-title {
		display: flex;
		align-items: center;
		gap: 10px;
		flex-wrap: wrap;
	}
	.favorite-title strong {
		font-size: 12px;
		color: #273247;
	}
	.favorite-title span {
		font-size: 9px;
		color: #a87a20;
		background: #fff7df;
		padding: 3px 7px;
		border-radius: 10px;
	}
	.favorite-main small {
		display: block;
		color: #8490a2;
		font-size: 10px;
		margin-top: 4px;
	}
	.clip-track {
		height: 4px;
		margin-top: 10px;
		background: #eff1f5;
		border-radius: 5px;
		overflow: hidden;
	}
	.clip-track i {
		display: block;
		width: 100%;
		height: 100%;
		background: linear-gradient(90deg, #806bde, #a694f1);
	}
	.actions {
		display: flex;
		gap: 8px;
		flex: none;
	}
	.empty-state {
		display: grid;
		justify-items: center;
		text-align: center;
		padding: 68px 20px;
		background: #fff;
		border: 1px dashed #dce2ea;
		border-radius: 13px;
	}
	.empty-icon {
		width: 54px;
		height: 54px;
		display: grid;
		place-items: center;
		border-radius: 16px;
		background: #fff7df;
		color: #d89b26;
		font-size: 25px;
	}
	.empty-state h2 {
		margin: 14px 0 5px;
		font-size: 16px;
	}
	.empty-state p {
		margin-bottom: 18px;
	}
	.error {
		border: 1px solid #f4d6da;
		background: #fff2f3;
		color: #a63242;
		padding: 11px 14px;
		border-radius: 9px;
		margin-bottom: 14px;
	}
	@media (max-width: 700px) {
		.favorite-card {
			align-items: flex-start;
			flex-wrap: wrap;
		}
		.favorite-icon {
			margin-top: 2px;
		}
		.favorite-main {
			width: calc(100% - 60px);
			flex: auto;
		}
		.actions {
			margin-left: 53px;
			flex-wrap: wrap;
		}
		.player-dialog {
			width: calc(100vw - 20px);
			max-width: none;
			padding: 12px;
		}
		.player-dialog-head {
			align-items: flex-start;
		}
		.player-dialog-head .button {
			flex: none;
		}
		.favorite-player video {
			max-height: 65vh;
		}
	}
</style>
