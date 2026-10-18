<script lang="ts">
	import { userFacingError } from '$lib/messages';
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { connectDvrEvents } from '$lib/events';
	import type { Stream } from '$lib/types';

	let streams = $state<Stream[]>([]);
	let active = $state<Record<string, boolean>>({});
	let error = $state('');

	async function load() {
		error = '';
		try {
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
		} catch (e) {
			error = userFacingError(e);
		}
	}
	onMount(() =>
		connectDvrEvents(
			{ 'stream.started': () => void load(), 'stream.stopped': () => void load() },
			(auth) => {
				if (auth.authenticated) void load();
				else {
					streams = [];
					active = {};
				}
			},
		),
	);
</script>

<section class="page-head">
	<div>
		<div class="eyebrow">我的频道</div>
		<h1>频道列表</h1>
		<p>进入频道即可观看直播、回看刚刚错过的内容，或浏览直播历史。</p>
	</div>
	<div class="head-actions">
		<a class="button secondary" href="/settings?section=channels">频道管理</a>
		<button class="button secondary" onclick={load}>刷新频道状态</button>
	</div>
</section>
{#if error}<div class="error">{error}</div>{/if}
{#if streams.length}
	<div class="channel-grid">
		{#each streams as stream}
			<article class="channel-card">
				<a class="channel-main" href={`/live?streamId=${encodeURIComponent(stream.id)}`}>
					<div class="channel-icon">◉</div>
					<div class="channel-info">
						<div class="name-line">
							<h2>{stream.name}</h2>
							<span class:online={active[stream.id]} class="status">
								<i></i>
								{active[stream.id] ? '直播中' : '未开播'}
							</span>
						</div>
						<p>创建于 {new Date(stream.createdAt).toLocaleString()}</p>
					</div>
					<span class="arrow">进入频道 →</span>
				</a>
			</article>
		{/each}
	</div>
{:else}
	<div class="empty-state">
		<div class="empty-icon">◉</div>
		<h2>还没有频道</h2>
		<p>创建频道后，就可以开始推流并自动保存直播录像。</p>
		<a class="button" href="/settings?section=channels">创建频道</a>
	</div>
{/if}

<style>
	.page-head {
		display: flex;
		justify-content: space-between;
		align-items: end;
		margin-bottom: 24px;
	}
	.head-actions {
		display: flex;
		align-items: center;
		gap: 8px;
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
	.channel-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(min(100%, 390px), 1fr));
		gap: 14px;
	}
	.channel-card {
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
		overflow: hidden;
	}
	.channel-main {
		display: flex;
		align-items: center;
		gap: 13px;
		padding: 20px;
		color: inherit;
		text-decoration: none;
	}
	.channel-main:hover {
		background: #faf9ff;
	}
	.channel-icon {
		width: 44px;
		height: 44px;
		display: grid;
		place-items: center;
		border-radius: 12px;
		background: #f0edff;
		color: #6955c9;
		font-size: 20px;
	}
	.channel-info {
		min-width: 0;
		flex: 1;
	}
	.name-line {
		display: flex;
		align-items: center;
		gap: 10px;
		flex-wrap: wrap;
	}
	h2 {
		font-size: 16px;
		color: #263143;
		margin: 0;
	}
	.channel-info p {
		font-size: 11px;
		margin-top: 5px;
	}
	.status {
		display: flex;
		align-items: center;
		gap: 6px;
		color: #8993a2;
		font-size: 10px;
	}
	.status i {
		width: 7px;
		height: 7px;
		border-radius: 100%;
		background: #adb6c4;
	}
	.status.online {
		color: #16835f;
	}
	.status.online i {
		background: #35bd8c;
	}
	.arrow {
		font-size: 11px;
		font-weight: 650;
		color: #6653c7;
		white-space: nowrap;
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
		background: #f0edff;
		color: #6955c9;
		font-size: 25px;
	}
	.empty-state h2 {
		margin: 14px 0 5px;
	}
	.empty-state .button {
		margin-top: 16px;
	}
	.error {
		border: 1px solid #f4d6da;
		background: #fff2f3;
		color: #a63242;
		padding: 11px 14px;
		border-radius: 9px;
		margin-bottom: 14px;
	}
	@media (max-width: 620px) {
		.page-head {
			align-items: flex-start;
			gap: 12px;
			flex-direction: column;
		}
		.head-actions {
			width: 100%;
		}
		.arrow {
			font-size: 0;
		}
		.arrow::after {
			content: '→';
			font-size: 16px;
		}
	}
</style>
