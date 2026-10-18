<script lang="ts">
	import { userFacingError, playbackErrorMessage } from '$lib/messages';
	import { onMount, tick } from 'svelte';
	import { api } from '$lib/api';
	import { connectDvrEvents } from '$lib/events';
	import type { Stream, TimeRange } from '$lib/types';

	let streams = $state<Stream[]>([]);
	let selected = $state('');
	let role = $state<'admin' | 'viewer' | ''>('');
	const localInput = (date: Date) =>
		new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
	const initialTimelineEnd = new Date(Math.floor(Date.now() / 60_000) * 60_000 + 60_000);
	let start = $state(localInput(new Date(initialTimelineEnd.getTime() - 24 * 3600_000)));
	let end = $state(localInput(initialTimelineEnd));
	let ranges = $state<TimeRange[]>([]);
	type TimelineBucket = { start: string; end: string; recordedMs: number; segmentCount: number };
	let buckets = $state<TimelineBucket[]>([]);
	let bucketGranularity = $state<'hour' | 'day' | ''>('');
	let busy = $state(false);
	let error = $state('');
	let playbackTitle = $state('');
	let playbackError = $state('');
	let video = $state<HTMLVideoElement>();
	let player: { destroy: () => void } | undefined;

	const iso = (value: string) => new Date(value).toISOString();
	const bucketLabel = (bucket: TimelineBucket) => {
		const value = new Date(bucket.start).toISOString();
		return bucketGranularity === 'day'
			? `${value.slice(0, 10)} UTC`
			: `${value.slice(0, 16).replace('T', ' ')} UTC`;
	};
	async function load() {
		if (!selected) return;
		busy = true;
		error = '';
		try {
			const startDate = new Date(start);
			const endDate = new Date(end);
			if (
				!Number.isFinite(startDate.getTime()) ||
				!Number.isFinite(endDate.getTime()) ||
				endDate <= startDate
			) {
				error = '请选择有效的时间范围，并确保结束时间晚于开始时间。';
				return;
			}
			const span = endDate.getTime() - startDate.getTime();
			const query = new URLSearchParams({ start: iso(start), end: iso(end) });
			if (span > 24 * 3600_000) {
				bucketGranularity = span > 7 * 24 * 3600_000 ? 'day' : 'hour';
				query.set('granularity', bucketGranularity);
				const result = await api<{ buckets: TimelineBucket[] }>(
					`/api/streams/${selected}/timeline?${query}`,
				);
				buckets = result.buckets;
				ranges = [];
			} else {
				bucketGranularity = '';
				const result = await api<{ ranges: TimeRange[] }>(
					`/api/streams/${selected}/timeline?${query}`,
				);
				ranges = result.ranges;
				buckets = [];
				bucketGranularity = '';
			}
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}

	async function drillInto(bucket: TimelineBucket) {
		start = localInput(new Date(bucket.start));
		end = localInput(new Date(bucket.end));
		await load();
	}

	async function playRange(range: TimeRange) {
		if (!selected) return;
		player?.destroy();
		playbackError = '';
		playbackTitle = `${new Date(range.start).toLocaleString()} — ${new Date(range.end).toLocaleString()}`;
		await tick();
		if (!video) return;
		const mediaElement = video;
		try {
			const resolveQuery = new URLSearchParams({ streamId: selected, at: range.start });
			const resolved = await api<{ offset: number }>(`/api/playback/resolve?${resolveQuery}`);
			const playlistQuery = new URLSearchParams({ start: range.start, end: range.end });
			const playlist = `/api/playback/dvr/${selected}.m3u8?${playlistQuery}`;
			const { default: Hls } = await import('hls.js');
			if (Hls.isSupported()) {
				const hls = new Hls({
					xhrSetup: (xhr) => {
						xhr.withCredentials = true;
					},
				});
				hls.on(Hls.Events.MANIFEST_PARSED, () => {
					mediaElement.currentTime = Math.max(0, resolved.offset);
					void mediaElement.play().catch(() => {});
				});
				hls.on(Hls.Events.ERROR, (_event, data) => {
					if (data.fatal) playbackError = playbackErrorMessage(data.details);
				});
				hls.loadSource(playlist);
				hls.attachMedia(mediaElement);
				player = hls;
			} else if (mediaElement.canPlayType('application/vnd.apple.mpegurl')) {
				mediaElement.src = playlist;
				mediaElement.onloadedmetadata = () => {
					mediaElement.currentTime = Math.max(0, resolved.offset);
					void mediaElement.play().catch(() => {});
				};
			} else {
				playbackError = '当前浏览器暂不支持播放此录像，请尝试使用 Chrome 或 Edge。';
			}
		} catch (e) {
			playbackError = userFacingError(e);
		}
	}

	async function toggleProtection(range: TimeRange) {
		try {
			await Promise.all(
				(range.segmentIds ?? []).map((id) =>
					api(`/api/media/${id}/pin`, {
						method: 'PUT',
						body: JSON.stringify({ pinned: !range.pinned }),
					}),
				),
			);
			await load();
		} catch (e) {
			error = userFacingError(e);
		}
	}

	function closePlayback() {
		player?.destroy();
		player = undefined;
		if (video) {
			video.pause();
			video.removeAttribute('src');
			video.load();
		}
		playbackTitle = '';
	}

	onMount(() => {
		const disconnect = connectDvrEvents({ 'timeline.updated': () => void load() }, (auth) => {
			role = auth.role ?? '';
			if (!auth.authenticated) {
				streams = [];
				selected = '';
				ranges = [];
				buckets = [];
				return;
			}
			void (async () => {
				try {
					const result = await api<{ streams: Stream[] }>('/api/streams');
					streams = result.streams ?? [];
					if (!streams.some((stream) => stream.id === selected)) selected = streams[0]?.id ?? '';
					if (selected) await load();
				} catch (e) {
					error = userFacingError(e);
				}
			})();
		});
		return () => {
			disconnect();
			player?.destroy();
		};
	});
</script>

<section class="head">
	<div>
		<div class="eyebrow">录像档案</div>
		<h1>录像时间轴</h1>
		<p>按时间范围查找已录制内容，并查看录像中断的位置。</p>
	</div>
</section>
<section class="panel filters">
	<label>
		频道
		<select bind:value={selected}>
			<option value="">选择频道</option>
			{#each streams as stream}<option value={stream.id}>{stream.name}</option>{/each}
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
	<button class="button" disabled={busy || !selected} onclick={load}>
		{busy ? '载入中…' : '查询时间轴'}
	</button>
</section>
{#if streams.length === 0}<div class="empty-state">
		还没有频道，暂时没有可查询的录像。
		<a href="/settings?section=channels">创建频道</a>
	</div>{/if}
{#if error}<div class="error">{error}</div>{/if}
{#if playbackTitle}<section class="panel playback">
		<div class="playback-head">
			<div>
				<h2>历史回放</h2>
				<small>{playbackTitle}</small>
			</div>
			<button class="button secondary" onclick={closePlayback}>关闭</button>
		</div>
		<video bind:this={video} controls playsinline></video>
		{#if playbackError}<p class="error">
				{playbackError}
			</p>{/if}
	</section>{/if}
<section class="panel timeline">
	<div class="timeline-head">
		<div>
			<h2>{buckets.length ? '录像概览' : '可播放区间'}</h2>
			<small>
				{buckets.length
					? `${buckets.length} 个时间桶 · 点击下钻查看细节`
					: `${ranges.filter((x) => x.available).length} 段媒体范围`}
			</small>
		</div>
		<span class="legend">
			<i></i>
			已录制
			<i class="gap"></i>
			中断 / 不可用
		</span>
	</div>
	{#if buckets.length}<div class="bucket-grid">
			{#each buckets as bucket}<button
					class="bucket"
					onclick={() => void drillInto(bucket)}
					title={`${bucketLabel(bucket)} · ${bucket.segmentCount} 个片段`}
				>
					<span>{bucketLabel(bucket)}</span>
					<i>
						<b
							style={`width:${Math.min(100, (bucket.recordedMs / Math.max(1, new Date(bucket.end).getTime() - new Date(bucket.start).getTime())) * 100)}%`}
						></b>
					</i>
					<small>
						{bucket.segmentCount
							? `${bucket.segmentCount} 段 · ${bucket.recordedMs >= 60000 ? `${Math.round(bucket.recordedMs / 60000)} 分钟` : `${Math.round(bucket.recordedMs / 1000)} 秒`}`
							: '无录像'}
					</small>
				</button>{/each}
		</div>{:else}<div class="track">
			<div class="ticks">
				<span>{new Date(start).toLocaleString()}</span>
				<span>{new Date(end).toLocaleString()}</span>
			</div>
			<div class="bar">
				{#each ranges as range}<div
						class:unavailable={!range.available}
						class="range"
						style={`left:${Math.max(0, ((new Date(range.start).getTime() - new Date(start).getTime()) / (new Date(end).getTime() - new Date(start).getTime())) * 100)}%;width:${Math.max(0.5, ((new Date(range.end).getTime() - new Date(start).getTime()) / (new Date(end).getTime() - new Date(start).getTime())) * 100)}%`}
						title={`${new Date(range.start).toLocaleString()} – ${new Date(range.end).toLocaleString()}${range.reason ? ` · ${range.reason}` : ''}`}
					></div>{/each}
			</div>
			<div class="ticks">
				<span>{new Date(start).toLocaleTimeString()}</span>
				<span>
					{new Date((new Date(start).getTime() + new Date(end).getTime()) / 2).toLocaleTimeString()}
				</span>
				<span>{new Date(end).toLocaleTimeString()}</span>
			</div>
		</div>
		{#if ranges.length === 0}<div class="empty">这个时间范围内没有可用录像。</div>{/if}{/if}
</section>
{#if !buckets.length}<section class="panel list">
		<h2>区间详情</h2>
		{#each ranges as range}<div class="row">
				<span class:good={range.available} class="range-dot"></span>
				<div>
					<strong>{range.available ? '已录制' : '不可用'}</strong>
					<small>{range.sessionId ?? range.reason ?? '无录像数据'}</small>
				</div>
				<time>
					{new Date(range.start).toLocaleString()} — {new Date(range.end).toLocaleString()}
				</time>
				<button
					class="button secondary"
					disabled={!range.available}
					onclick={() => playRange(range)}
				>
					回放
				</button>
				{#if role === 'admin' && range.available}<button
						class="button secondary"
						onclick={() => void toggleProtection(range)}
					>
						{range.pinned ? '取消保护' : '保护录像'}
					</button>{/if}{#if role === 'admin'}<a
						class="button secondary"
						href={`/clips?streamId=${encodeURIComponent(selected)}&startTime=${encodeURIComponent(range.start)}&endTime=${encodeURIComponent(range.end)}`}
					>
						导出这段
					</a>{/if}
			</div>{/each}
	</section>{/if}

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
		margin-bottom: 22px;
	}
	.filters {
		display: grid;
		grid-template-columns: minmax(150px, 1fr) minmax(190px, 1.2fr) minmax(190px, 1.2fr) auto;
		align-items: end;
	}
	.timeline,
	.playback,
	.list {
		padding: 20px;
		margin-top: 16px;
	}
	.timeline-head,
	.playback-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
	}
	.timeline-head {
		padding-bottom: 15px;
		border-bottom: 1px solid #edf0f4;
	}
	.timeline-head h2,
	.list h2 {
		margin: 0;
	}
	.timeline-head small,
	.playback-head small {
		display: block;
		color: #788496;
		font-size: 11px;
		margin-top: 4px;
	}
	.legend {
		display: flex;
		align-items: center;
		gap: 7px;
		color: #687487;
		font-size: 10px;
		white-space: nowrap;
	}
	.legend i {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: #36b987;
		display: inline-block;
	}
	.legend i.gap {
		background: #cbd2dc;
		margin-left: 6px;
	}
	.track {
		padding: 22px 2px 7px;
	}
	.ticks {
		display: flex;
		justify-content: space-between;
		gap: 12px;
		color: #8993a3;
		font-size: 10px;
	}
	.ticks + .bar {
		margin: 9px 0;
	}
	.bar {
		position: relative;
		height: 18px;
		background: #edf0f4;
		border: 1px solid #e4e8ef;
		border-radius: 7px;
		overflow: hidden;
	}
	.range {
		position: absolute;
		top: 2px;
		bottom: 2px;
		min-width: 4px;
		background: linear-gradient(90deg, #5dbb9c, #2fa77e);
		border-radius: 4px;
	}
	.range.unavailable {
		background: #bac3d0;
	}
	.row {
		display: grid;
		grid-template-columns: 10px minmax(130px, 1fr) minmax(200px, auto) auto auto auto;
		align-items: center;
		gap: 13px;
		border-top: 1px solid #edf0f4;
		padding: 14px 0;
	}
	.range-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: #bac3d0;
	}
	.range-dot.good {
		background: #36b987;
	}
	.row > div {
		display: grid;
		gap: 3px;
	}
	.row strong {
		font-size: 12px;
	}
	.row small {
		font-size: 10px;
	}
	.row time {
		font-size: 11px;
		white-space: nowrap;
	}
	.playback-head {
		margin-bottom: 13px;
	}
	.playback video {
		display: block;
		width: 100%;
		max-height: 65vh;
		background: #10141c;
		border-radius: 9px;
	}
	@media (max-width: 1050px) {
		.filters {
			grid-template-columns: repeat(2, minmax(0, 1fr));
		}
		.row {
			grid-template-columns: 10px minmax(100px, 1fr) auto auto;
		}
		.row time {
			grid-column: 2/-1;
			grid-row: 2;
			white-space: normal;
		}
		.row > button,
		.row > a {
			grid-row: 1;
		}
	}
	@media (max-width: 620px) {
		.filters {
			grid-template-columns: 1fr;
		}
		.timeline,
		.playback,
		.list {
			padding: 15px;
		}
		.timeline-head {
			align-items: flex-start;
			flex-direction: column;
		}
		.row {
			grid-template-columns: 10px minmax(0, 1fr) auto;
			gap: 10px;
		}
		.row time {
			grid-column: 2/-1;
			grid-row: 2;
		}
		.row > button,
		.row > a {
			grid-row: auto;
			grid-column: 2/-1;
		}
		.ticks {
			font-size: 9px;
		}
	}
	.bucket-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(170px, 1fr));
		gap: 8px;
		margin-top: 17px;
	}
	.bucket {
		display: grid;
		gap: 7px;
		text-align: left;
		background: #0d1119;
		border: 1px solid #2a3342;
		border-radius: 6px;
		padding: 10px;
		color: #dce2eb;
		cursor: pointer;
	}
	.bucket:hover {
		border-color: #826fe3;
		background: #171c26;
	}
	.bucket span,
	.bucket small {
		font-size: 10px;
	}
	.bucket small {
		color: #8994a6;
	}
	.bucket i {
		height: 7px;
		background: #252d3a;
		border-radius: 5px;
		overflow: hidden;
	}
	.bucket b {
		display: block;
		height: 100%;
		background: linear-gradient(90deg, #7963d9, #aa8dfb);
	}
</style>
