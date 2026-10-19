<script lang="ts">
	import { userFacingError, playbackErrorMessage, clipStatusLabel } from '#lib/messages';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '#lib/api';
	import { connectDvrEvents } from '#lib/events';
	import {
		browserPublish,
		startBrowserPublish as startBrowserPublishSession,
		stopBrowserPublish as stopBrowserPublishSession,
	} from '#lib/browser-publish.svelte';
	import type { ClipJob, Stream, TimeRange } from '#lib/types';
	type PlaybackPlayer = {
		destroy: () => void;
		currentProgramDateTime?: Date | null;
		liveSyncPosition?: number | null;
		currentLevel?: number;
		levels?: Array<{
			bitrate: number;
			width?: number;
			height?: number;
			videoCodec?: string;
			audioCodec?: string;
		}>;
		bandwidthEstimate?: number;
		latency?: number;
		mainForwardBufferInfo?: { len: number } | null;
	};
	let streams = $state<Stream[]>([]);
	let active = $state<Record<string, boolean>>({});
	let streamInterrupted = $state(false);
	let selected = $state('');
	let historyTab = $derived(page.url.searchParams.get('tab') === 'history');
	let lastPlayerTab: boolean | undefined;
	let selectedStream = $derived(streams.find((stream) => stream.id === selected));
	let role = $state<'admin' | 'viewer' | ''>('');
	let mode = $state<'live' | 'dvr'>('live');
	let liveLatencyTargetSeconds = $state(6);
	let playbackSettingsPromise: Promise<void> | undefined;
	let video: HTMLVideoElement;
	let player: PlaybackPlayer | undefined;
	let error = $state('');
	let status = $state('正在连接媒体服务…');
	let busy = $state(false);
	let streamName = $state('');
	let revealKey = $state('');
	let browserPushOpen = $state(false);
	let browserPushKey = $state('');
	let browserPushMode = $state<'screen' | 'camera'>('screen');
	let browserPushFrameRate = $state(60);
	let browserPushResolution = $state<'1080p' | '720p' | 'original'>('1080p');
	let pushDialogOpen = $state(false);
	let pushDialogView = $state<'options' | 'obs'>('options');
	let pushKeyInfo = $state<
		{ publishKey: string; obs: { server: string; streamKey: string }; rotated: boolean } | undefined
	>();
	let pushKeyBusy = $state(false);
	let pushKeyError = $state('');
	let copiedPushField = $state('');
	let mutedPlayback = $state(true);
	let isPlaying = $state(false);
	let livePoster = $state('');
	let livePosterLoading = $state(false);
	let livePosterStreamId = '';
	let livePlaybackStarted = $state(false);
	let posterPreviewVideo: HTMLVideoElement | undefined;
	let posterPreviewPlayer: PlaybackPlayer | undefined;
	let posterPreviewTimer = 0;
	let posterPreviewAttempt = 0;
	let currentTime = $state(0);
	let timelineStart = $state(0);
	let timelineEnd = $state(0);
	let liveTarget = $state(0);
	let videoPanel: HTMLElement;
	let screenPanel: HTMLElement;
	let webFullscreen = $state(false);
	let fullscreenActive = $state(false);
	let screenFullscreen = $state(false);
	let controlsVisible = $state(true);
	let controlsHideTimer = 0;
	let liveStatusTimer = 0;
	let debugOpen = $state(false);
	let debugMenuOpen = $state(false);
	let debugMenuX = $state(0);
	let debugMenuY = $state(0);
	let debugMenu = $state<HTMLDivElement | undefined>();
	let debugTimer = 0;
	let playbackEngine = $state('—');
	let debugLastError = $state('无');
	let debugStats = $state({
		state: '—',
		resolution: '—',
		level: '—',
		bandwidth: '—',
		buffer: '—',
		latency: '—',
		fps: '—',
		frames: '—',
	});
	let lastFrameSample: { at: number; frames: number } | undefined;
	let nativeVideoFullscreen = false;
	let atLiveEdge = $derived(timelineEnd > 0 && liveTarget > 0 && currentTime >= liveTarget - 3);
	let liveLatencySeconds = $derived(
		player?.latency && Number.isFinite(player.latency)
			? player.latency
			: timelineEnd > 0 && Number.isFinite(currentTime)
				? Math.max(0, timelineEnd - currentTime)
				: undefined,
	);
	let historyRanges = $state<TimeRange[]>([]);
	let historyRangesNewestFirst = $derived([...historyRanges].reverse());
	let historyBusy = $state(false);
	let historyDay = $state(toLocalDateInput(new Date()));
	let historyError = $state('');
	let selectedRange = $state<TimeRange | undefined>();
	let historyJumpAt = page.url.searchParams.get('at') ?? '';
	let playbackOffset = 0;
	let playbackStartAt = '';
	let playbackTimeMap: Array<{ mediaStart: number; mediaEnd: number; wallStart: number }> = [];
	let clipStart = $state('');
	let clipEnd = $state('');
	let clipBusy = $state(false);
	let clipError = $state('');
	let clipEncodingMode = $state('fast');
	let clipQualityPreset = $state('balanced');
	let clipResolution = $state('720p');
	let clipRefreshTimer = 0;
	let clipRefreshBusy = false;
	let clipSettingsDialogOpen = $state(false);
	let clipDialogFavorite = $state(false);
	let favoriteName = $state('');
	let clipJob = $state<ClipJob | undefined>();
	let favoriteAfterExport = false;
	let playbackAttempt = 0;
	$effect(() => {
		const requested = page.url.searchParams.get('streamId');
		if (requested && requested !== selected) void refresh();
	});
	$effect(() => {
		if (lastPlayerTab === undefined) {
			lastPlayerTab = historyTab;
			return;
		}
		if (lastPlayerTab !== historyTab) {
			lastPlayerTab = historyTab;
			stopLivePosterPreview();
			livePoster = '';
			livePosterStreamId = '';
			livePlaybackStarted = false;
			selectedRange = undefined;
			mode = 'live';
			video?.pause();
			player?.destroy();
			player = undefined;
			if (video) {
				video.removeAttribute('src');
				video.load();
			}
			isPlaying = false;
			if (!historyTab && selected && active[selected]) void loadLivePoster(selected);
			return;
		}
		if (historyTab && !selectedRange) {
			mode = 'live';
		} else if (!historyTab && selected && active[selected] && !livePlaybackStarted) {
			void loadLivePoster(selected);
		}
	});
	const autoplayBlockedMessage = '浏览器阻止了自动播放，请点击播放按钮开始播放。';
	const playbackFailedMessage = '暂时无法播放，请检查频道推流状态后重试。';
	async function refresh() {
		try {
			const previousSelected = selected;
			const previousLive = active[previousSelected];
			const session = await api<{ authenticated: boolean }>('/api/auth/session');
			if (!session.authenticated) {
				status = '登录状态已失效';
				await goto(
					`/login?next=${encodeURIComponent(window.location.pathname + window.location.search)}`,
				);
				return;
			}
			const data = await api<{ streams: Stream[] }>('/api/streams');
			streams = data.streams ?? [];
			const requested = new URLSearchParams(window.location.search).get('streamId');
			if (!requested || !streams.some((stream) => stream.id === requested)) {
				await goto('/channels', { replaceState: true });
				return;
			}
			selected = requested;
			const states = await Promise.all(
				streams.map(async (stream) => {
					try {
						return [
							stream.id,
							(await api<{ live: boolean }>(`/api/streams/${stream.id}/status`)).live,
							true,
						] as const;
					} catch {
						return [stream.id, false, false] as const;
					}
				}),
			);
			const nextActive = Object.fromEntries(states.map(([id, live]) => [id, live]));
			const selectedStatusConfirmed = states.find(([id]) => id === selected)?.[2] ?? false;
			if (selected !== previousSelected) {
				streamInterrupted = false;
			} else if (selectedStatusConfirmed && nextActive[selected]) {
				streamInterrupted = false;
			} else if (selectedStatusConfirmed && previousLive && !nextActive[selected]) {
				streamInterrupted = true;
			}
			active = nextActive;
			status = '服务连接正常';
			error = '';
			if (
				!historyTab &&
				selected &&
				(selected !== previousSelected ||
					!player ||
					(mode === 'live' && previousLive !== active[selected]))
			) {
				if (mode === 'live') {
					if (selected !== previousSelected) {
						livePlaybackStarted = false;
						livePoster = '';
						livePosterStreamId = '';
						stopLivePosterPreview();
					}
					if (active[selected]) void loadLivePoster(selected);
					else {
						stopLivePosterPreview();
						livePoster = '';
						livePosterStreamId = '';
					}
				} else void play();
			}
			if (selected) void loadHistory();
		} catch (e) {
			error = userFacingError(e);
			status = '连接失败';
		}
	}
	async function refreshLiveStatus() {
		const streamId = selected;
		if (!streamId || historyTab) return;
		try {
			const { live } = await api<{ live: boolean }>(`/api/streams/${streamId}/status`);
			if (selected !== streamId) return;
			const wasLive = active[streamId] === true;
			active = { ...active, [streamId]: live };
			if (live) {
				streamInterrupted = false;
				if (mode === 'live' && !livePlaybackStarted) void loadLivePoster(streamId);
			} else if (wasLive) {
				streamInterrupted = true;
				stopLivePosterPreview();
				livePoster = '';
				livePosterStreamId = '';
			}
		} catch {
			// Keep the last confirmed state when the status request temporarily fails.
		}
	}
	async function addStream() {
		busy = true;
		error = '';
		try {
			const data = await api<{ stream: Stream }>('/api/streams', {
				method: 'POST',
				body: JSON.stringify({ name: streamName }),
			});
			streams = [data.stream, ...streams];
			selected = data.stream.id;
			revealKey = data.stream.publishKey ?? '';
			streamName = '';
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}
	async function toggleStream() {
		if (!selectedStream) return;
		busy = true;
		error = '';
		try {
			const value = await api<Stream>(`/api/streams/${selectedStream.id}/enabled`, {
				method: 'PUT',
				body: JSON.stringify({ enabled: !selectedStream.enabled }),
			});
			streams = streams.map((stream) => (stream.id === value.id ? value : stream));
		} catch (e) {
			error = userFacingError(e);
		} finally {
			busy = false;
		}
	}
	function openPushDialog() {
		pushDialogView = 'options';
		pushKeyInfo = undefined;
		pushKeyError = '';
		pushDialogOpen = true;
	}
	function toggleInPageFullscreen(event: MouseEvent) {
		event.preventDefault();
		webFullscreen = !webFullscreen;
		revealPlayerControls();
	}
	function handleScreenClick() {
		if (fullscreenActive || webFullscreen) hidePlayerControls();
	}
	async function handleVideoDoubleClick(event: MouseEvent) {
		event.preventDefault();
		if (webFullscreen) {
			webFullscreen = false;
			return;
		}
		if (document.fullscreenElement) {
			await document.exitFullscreen();
			return;
		}
		if (nativeVideoFullscreen) {
			const nativeVideo = video as HTMLVideoElement & { webkitExitFullscreen?: () => void };
			nativeVideo.webkitExitFullscreen?.();
			return;
		}
		toggleInPageFullscreen(event);
	}
	async function loadPushKey() {
		if (!selected) return undefined;
		pushKeyBusy = true;
		pushKeyError = '';
		try {
			pushKeyInfo = await api<{
				publishKey: string;
				obs: { server: string; streamKey: string };
				rotated: boolean;
			}>(`/api/streams/${selected}/publish-key`);
			return pushKeyInfo;
		} catch (e) {
			pushKeyError = userFacingError(e);
			return undefined;
		} finally {
			pushKeyBusy = false;
		}
	}
	async function showObsPushInfo() {
		pushDialogView = 'obs';
		await loadPushKey();
	}
	async function startBrowserPush() {
		const info = await loadPushKey();
		if (!info) return;
		pushDialogOpen = false;
		browserPushKey = info.publishKey;
		browserPublish.error = '';
		browserPushOpen = true;
		browserPushMode = 'screen';
	}
	async function copyPushField(value: string, field: string) {
		try {
			await navigator.clipboard.writeText(value);
			copiedPushField = field;
			window.setTimeout(() => {
				if (copiedPushField === field) copiedPushField = '';
			}, 1800);
		} catch {
			pushKeyError = '复制失败，请手动选择并复制推流信息';
		}
	}
	async function startBrowserPublish() {
		if (!selected || !browserPushKey.trim()) return;
		await startBrowserPublishSession({
			streamId: selected,
			publishKey: browserPushKey,
			mode: browserPushMode,
			frameRate: browserPushFrameRate,
			resolution: browserPushResolution,
		});
		if (browserPublish.active) {
			browserPushKey = '';
			browserPushOpen = false;
			void refresh();
		}
	}
	async function stopBrowserPublish() {
		await stopBrowserPublishSession();
		browserPublish.error = '';
		void refresh();
	}
	async function waitForLiveSegment(streamId: string, attempt: number) {
		const deadline = Date.now() + 18_000;
		while (Date.now() < deadline) {
			if (
				attempt !== playbackAttempt ||
				selected !== streamId ||
				mode !== 'live' ||
				!active[streamId]
			)
				return false;
			try {
				const response = await fetch(`/api/playback/live/${streamId}.m3u8`, {
					credentials: 'same-origin',
					cache: 'no-store',
				});
				if (response.ok && (await response.text()).includes('#EXTINF:')) return true;
			} catch {
				// Retry while the publisher becomes ready.
			}
			await new Promise((resolve) => window.setTimeout(resolve, 750));
		}
		return false;
	}
	function stopLivePosterPreview() {
		posterPreviewAttempt++;
		window.clearTimeout(posterPreviewTimer);
		posterPreviewVideo?.pause();
		posterPreviewPlayer?.destroy();
		posterPreviewVideo = undefined;
		posterPreviewPlayer = undefined;
		livePosterLoading = false;
	}
	async function loadLivePoster(streamId: string) {
		if (
			!streamId ||
			!active[streamId] ||
			historyTab ||
			isPlaying ||
			livePosterLoading ||
			(livePosterStreamId === streamId && livePoster)
		)
			return;
		stopLivePosterPreview();
		const attempt = posterPreviewAttempt;
		livePosterStreamId = streamId;
		livePoster = '';
		livePosterLoading = true;
		const previewVideo = document.createElement('video');
		previewVideo.muted = true;
		previewVideo.playsInline = true;
		posterPreviewVideo = previewVideo;
		const capture = () => {
			if (attempt !== posterPreviewAttempt || !previewVideo.videoWidth || !previewVideo.videoHeight)
				return;
			try {
				const canvas = document.createElement('canvas');
				canvas.width = previewVideo.videoWidth;
				canvas.height = previewVideo.videoHeight;
				const context = canvas.getContext('2d');
				if (!context) return;
				context.drawImage(previewVideo, 0, 0, canvas.width, canvas.height);
				livePoster = canvas.toDataURL('image/jpeg', 0.82);
			} catch {
				// A preview frame is best-effort; playback remains available without it.
			} finally {
				stopLivePosterPreview();
			}
		};
		const captureAfterFrame = () => {
			if ('requestVideoFrameCallback' in previewVideo)
				previewVideo.requestVideoFrameCallback(() => capture());
			else window.setTimeout(capture, 180);
		};
		previewVideo.addEventListener('playing', captureAfterFrame, { once: true });
		posterPreviewTimer = window.setTimeout(() => {
			if (attempt !== posterPreviewAttempt) return;
			livePosterStreamId = '';
			stopLivePosterPreview();
		}, 10_000);
		try {
			const { default: Hls } = await import('hls.js');
			if (attempt !== posterPreviewAttempt || selected !== streamId || !active[streamId]) return;
			if (Hls.isSupported()) {
				const hls = new Hls({
					xhrSetup: (xhr) => {
						xhr.withCredentials = true;
					},
				});
				posterPreviewPlayer = hls as unknown as PlaybackPlayer;
				hls.on(Hls.Events.MANIFEST_PARSED, () => {
					if (attempt === posterPreviewAttempt)
						void previewVideo.play().catch(() => {
							livePosterStreamId = '';
							stopLivePosterPreview();
						});
				});
				hls.loadSource(`/api/playback/live/${streamId}.m3u8`);
				hls.attachMedia(previewVideo);
			} else if (previewVideo.canPlayType('application/vnd.apple.mpegurl')) {
				previewVideo.src = `/api/playback/live/${streamId}.m3u8`;
				void previewVideo.play().catch(() => {
					livePosterStreamId = '';
					stopLivePosterPreview();
				});
			} else {
				livePosterStreamId = '';
				stopLivePosterPreview();
			}
		} catch {
			livePosterStreamId = '';
			stopLivePosterPreview();
		}
	}
	async function play() {
		if (!selected || !video) return;
		stopLivePosterPreview();
		const attempt = ++playbackAttempt;
		const streamId = selected;
		const playbackMode = mode;
		video.pause();
		player?.destroy();
		player = undefined;
		video.removeAttribute('src');
		video.load();
		mutedPlayback = false;
		video.muted = mutedPlayback;
		isPlaying = false;
		currentTime = 0;
		timelineStart = 0;
		timelineEnd = 0;
		liveTarget = 0;
		error = '';
		playbackEngine = '加载中';
		debugLastError = '无';
		if (playbackMode === 'live' && !active[streamId]) return;
		if (playbackMode === 'live') {
			playbackEngine = '等待首段画面';
			status = '推流已连接，正在等待首段可播放内容…';
			const ready = await waitForLiveSegment(streamId, attempt);
			if (attempt !== playbackAttempt || selected !== streamId || mode !== playbackMode) return;
			if (!active[streamId]) return;
			if (!ready) {
				playbackEngine = '等待推流内容';
				error = '推流已连接，但暂时还没有可播放画面，请稍后重试。';
				return;
			}
		}
		let url = `/api/playback/${playbackMode}/${streamId}.m3u8`;
		try {
			if (playbackMode === 'live') await loadPlaybackSettings();
			if (attempt !== playbackAttempt || selected !== streamId || mode !== playbackMode) return;
			if (selectedRange) {
				const startAt = playbackStartAt || selectedRange.start;
				const resolved = await api<{ offset: number }>(
					`/api/playback/resolve?${new URLSearchParams({ streamId, at: startAt })}`,
				);
				playbackOffset = resolved.offset;
				url += `?${new URLSearchParams({ start: startAt, end: selectedRange.end })}`;
			}
			const { default: Hls } = await import('hls.js');
			if (attempt !== playbackAttempt || selected !== streamId || mode !== playbackMode) return;
			if (Hls.isSupported()) {
				playbackEngine = 'hls.js (MSE)';
				const mediaElement = video;
				const liveOptions =
					playbackMode === 'live'
						? {
								liveSyncDuration: liveLatencyTargetSeconds,
								liveMaxLatencyDuration: liveLatencyTargetSeconds * 3,
							}
						: {};
				const hls = new Hls({
					...liveOptions,
					xhrSetup: (xhr) => {
						xhr.withCredentials = true;
					},
				});
				hls.on(Hls.Events.MANIFEST_PARSED, () => {
					if (attempt !== playbackAttempt) return;
					if (selectedRange) mediaElement.currentTime = playbackOffset;
					void mediaElement.play().catch(() => {
						error = autoplayBlockedMessage;
					});
				});
				hls.on(Hls.Events.ERROR, (_event, data) => {
					if (attempt !== playbackAttempt) return;
					debugLastError = `${data.type} / ${data.details}${data.fatal ? ' / fatal' : ''}`;
					if (data.fatal)
						error =
							data.details === 'levelEmptyError'
								? selectedRange
									? '所选区间内没有可播放内容，请重新选择直播历史。'
									: '暂时没有可播放内容，请确认频道正在推流后重试。'
								: playbackErrorMessage(data.details);
				});
				hls.loadSource(url);
				hls.attachMedia(mediaElement);
				player = hls as unknown as PlaybackPlayer;
			} else if (video.canPlayType('application/vnd.apple.mpegurl')) {
				playbackEngine = '原生 HLS';
				video.src = url;
				video.onloadedmetadata = () => {
					if (attempt !== playbackAttempt) return;
					if (selectedRange) video.currentTime = playbackOffset;
					void video.play().catch(() => {
						error = autoplayBlockedMessage;
					});
				};
			} else error = '当前浏览器暂不支持播放此直播。';
		} catch (e) {
			error = userFacingError(e);
			debugLastError = userFacingError(e);
		}
	}
	function updateTimeline() {
		if (!video) return;
		currentTime = Number.isFinite(video.currentTime) ? video.currentTime : 0;
		if (video.seekable.length) {
			const last = video.seekable.length - 1;
			timelineStart = video.seekable.start(last);
			timelineEnd = video.seekable.end(last);
		} else if (mode === 'dvr' && Number.isFinite(video.duration)) {
			timelineStart = 0;
			timelineEnd = video.duration;
		} else {
			timelineStart = 0;
			timelineEnd = 0;
		}
		const syncPosition = player?.liveSyncPosition;
		liveTarget =
			Number.isFinite(syncPosition) && syncPosition! > 0
				? syncPosition!
				: timelineEnd > 0
					? Math.max(timelineStart, timelineEnd - liveLatencyTargetSeconds)
					: 0;
	}
	function loadPlaybackSettings() {
		playbackSettingsPromise ??= (async () => {
			try {
				const settings = await api<{ liveLatencyTargetSeconds: number }>(
					'/api/system/playback',
				);
				if (Number.isFinite(settings.liveLatencyTargetSeconds) && settings.liveLatencyTargetSeconds > 0) {
					liveLatencyTargetSeconds = settings.liveLatencyTargetSeconds;
				}
			} catch {
				// Keep the UI fallback aligned with the shipped config if settings cannot load.
			}
		})();
		return playbackSettingsPromise;
	}
	function seekTo(value: number) {
		if (!video || timelineEnd <= timelineStart) return;
		video.currentTime = Math.max(timelineStart, Math.min(timelineEnd, value));
		updateTimeline();
	}
	function skipPlayback(seconds: number) {
		if (!video) return;
		seekTo(video.currentTime + seconds);
	}
	function hidePlayerControls() {
		if (isPlaying && (fullscreenActive || webFullscreen)) controlsVisible = false;
	}
	function revealPlayerControls() {
		controlsVisible = true;
		window.clearTimeout(controlsHideTimer);
		if (isPlaying && (fullscreenActive || webFullscreen))
			controlsHideTimer = window.setTimeout(hidePlayerControls, 2400);
	}
	function formatBitrate(value?: number) {
		return value && value > 0 ? `${(value / 1_000_000).toFixed(2)} Mbps` : '—';
	}
	function updateDebugStats() {
		if (!debugOpen || !video) return;
		const quality = video.getVideoPlaybackQuality?.();
		const now = performance.now();
		const totalFrames = quality?.totalVideoFrames;
		let fps = '—';
		if (typeof totalFrames === 'number') {
			if (lastFrameSample && now > lastFrameSample.at) {
				fps = `${Math.max(0, Math.round(((totalFrames - lastFrameSample.frames) * 1000) / (now - lastFrameSample.at)))} fps`;
			}
			lastFrameSample = { at: now, frames: totalFrames };
		}
		let bufferedAhead = 0;
		for (let index = 0; index < video.buffered.length; index++) {
			if (
				video.currentTime >= video.buffered.start(index) &&
				video.currentTime <= video.buffered.end(index)
			) {
				bufferedAhead = video.buffered.end(index) - video.currentTime;
				break;
			}
		}
		const levelIndex = player?.currentLevel ?? -1;
		const level = levelIndex >= 0 ? player?.levels?.[levelIndex] : undefined;
		const droppedFrames = quality?.droppedVideoFrames;
		const hlsBuffer = player?.mainForwardBufferInfo?.len;
		debugStats = {
			state: isPlaying ? '播放中' : video.paused ? '已暂停' : '就绪',
			resolution:
				video.videoWidth && video.videoHeight ? `${video.videoWidth} × ${video.videoHeight}` : '—',
			level: level
				? `${level.width ?? '?'} × ${level.height ?? '?'} · ${formatBitrate(level.bitrate)}`
				: playbackEngine === 'hls.js (MSE)' && levelIndex === -1
					? '自动选择'
					: '—',
			bandwidth: formatBitrate(player?.bandwidthEstimate),
			buffer: `${Math.max(0, hlsBuffer ?? bufferedAhead).toFixed(1)} 秒`,
			latency:
				player?.latency && Number.isFinite(player.latency)
					? `${player.latency.toFixed(1)} 秒`
					: '—',
			fps,
			frames:
				typeof droppedFrames === 'number' && typeof totalFrames === 'number'
					? `${droppedFrames} / ${totalFrames}`
					: '—',
		};
	}
	function closeDebugStats() {
		debugOpen = false;
		lastFrameSample = undefined;
		window.clearInterval(debugTimer);
	}
	function toggleDebugStats() {
		if (debugOpen) {
			closeDebugStats();
			return;
		}
		debugOpen = true;
		lastFrameSample = undefined;
		updateDebugStats();
		window.clearInterval(debugTimer);
		debugTimer = window.setInterval(updateDebugStats, 1000);
	}
	function handlePlayerContextMenu(event: MouseEvent) {
		event.preventDefault();
		const bounds = videoPanel.getBoundingClientRect();
		debugMenuX = Math.max(8, Math.min(event.clientX - bounds.left, bounds.width - 190));
		debugMenuY = Math.max(8, Math.min(event.clientY - bounds.top, bounds.height - 54));
		debugMenuOpen = true;
	}
	function handlePlayerPointerDown(event: PointerEvent) {
		revealPlayerControls();
		if (debugMenu && !debugMenu.contains(event.target as Node)) debugMenuOpen = false;
	}
	function handleMediaError() {
		if (video?.error) debugLastError = `MediaError ${video.error.code}`;
	}
	function handleVideoPlay() {
		isPlaying = true;
		if (!historyTab) {
			status = mode === 'live' ? '直播播放中' : '时移回看中';
			if (mode === 'live') livePlaybackStarted = true;
		}
		if (error === autoplayBlockedMessage || error === playbackFailedMessage) error = '';
		revealPlayerControls();
	}
	function handleVideoPause() {
		isPlaying = false;
		window.clearTimeout(controlsHideTimer);
		controlsVisible = true;
	}
	function togglePlayback() {
		if (!video) return;
		if (video.paused) {
			if (!video.currentSrc && !selectedRange) {
				void play();
				return;
			}
			error = '';
			void video.play().catch(() => {
				error = playbackFailedMessage;
			});
		} else video.pause();
	}
	function toggleMute() {
		mutedPlayback = !mutedPlayback;
		if (video) {
			video.muted = mutedPlayback;
			if (!mutedPlayback)
				void video.play().catch(() => {
					error = '浏览器未能开启声音，请检查设备音量与播放设置。';
				});
		}
	}
	function goLive() {
		if (timelineEnd > 0) seekTo(liveTarget > 0 ? liveTarget : timelineEnd - 10);
		else void play();
	}
	async function toggleFullscreen() {
		if (webFullscreen) {
			webFullscreen = false;
			return;
		}
		const nativeVideo = video as HTMLVideoElement & {
			webkitEnterFullscreen?: () => void;
			webkitExitFullscreen?: () => void;
		};
		if (document.fullscreenElement) {
			await document.exitFullscreen();
			return;
		}
		if (nativeVideoFullscreen) {
			nativeVideo.webkitExitFullscreen?.();
			return;
		}
		try {
			if (videoPanel?.requestFullscreen) await videoPanel.requestFullscreen();
			else if (nativeVideo.webkitEnterFullscreen) nativeVideo.webkitEnterFullscreen();
			else webFullscreen = true;
		} catch {
			if (nativeVideo.webkitEnterFullscreen) nativeVideo.webkitEnterFullscreen();
			else webFullscreen = true;
		}
	}
	function handleKeyboardShortcuts(event: KeyboardEvent) {
		if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return;
		const target = event.target;
		if (
			target instanceof HTMLElement &&
			(target.isContentEditable || target.matches('input,textarea,select'))
		)
			return;
		const key = event.key.toLowerCase();
		if (key === 'd') {
			event.preventDefault();
			debugMenuOpen = false;
			toggleDebugStats();
			return;
		}
		if (event.key === 'Escape' && debugMenuOpen) {
			event.preventDefault();
			debugMenuOpen = false;
			return;
		}
		if (event.key === 'Escape' && debugOpen) {
			event.preventDefault();
			closeDebugStats();
			return;
		}
		revealPlayerControls();
		if (event.key === 'Escape' && webFullscreen) {
			event.preventDefault();
			webFullscreen = false;
			return;
		}
		if (!selected) return;
		if (event.code === 'Space' || key === 'k') {
			event.preventDefault();
			if (!(mode === 'live' && !active[selected])) togglePlayback();
			return;
		}
		if (key === 'm') {
			event.preventDefault();
			toggleMute();
			return;
		}
		if (key === 'f') {
			event.preventDefault();
			void toggleFullscreen();
			return;
		}
		if (key === 'arrowleft' || key === 'arrowright') {
			if (timelineEnd <= timelineStart) return;
			event.preventDefault();
			const amount = event.shiftKey ? 10 : 5;
			seekTo(currentTime + (key === 'arrowleft' ? -amount : amount));
		}
	}
	function handleFullscreenChange() {
		screenFullscreen = document.fullscreenElement === videoPanel;
		fullscreenActive = screenFullscreen || nativeVideoFullscreen;
		revealPlayerControls();
	}

	function formatLocalInput(value: string, precision: number) {
		const date = new Date(value);
		return new Date(date.getTime() - date.getTimezoneOffset() * 60000)
			.toISOString()
			.slice(0, precision);
	}
	function toLocalDateInput(value: Date) {
		const local = new Date(value.getTime() - value.getTimezoneOffset() * 60_000);
		return local.toISOString().slice(0, 10);
	}
	function toLocalInput(value: string) {
		return formatLocalInput(value, 16);
	}
	function toPreciseLocalInput(value: string) {
		return formatLocalInput(value, 23);
	}
	function historyDayBounds(day = historyDay) {
		const start = new Date(`${day}T00:00:00`);
		const end = new Date(start);
		end.setDate(end.getDate() + 1);
		return { start, end };
	}
	function historyDisplayBounds() {
		const day = historyDayBounds();
		if (!historyRanges.length) return day;
		const dayStart = day.start.getTime();
		const dayEnd = day.end.getTime();
		const start = Math.max(
			dayStart,
			Math.min(...historyRanges.map((range) => new Date(range.start).getTime())),
		);
		const end = Math.min(
			dayEnd,
			Math.max(...historyRanges.map((range) => new Date(range.end).getTime())),
		);
		return end > start ? { start: new Date(start), end: new Date(end) } : day;
	}
	function historyTickLabel(position: number) {
		const { start, end } = historyDisplayBounds();
		const at = new Date(start.getTime() + (end.getTime() - start.getTime()) * position);
		return new Intl.DateTimeFormat('zh-CN', {
			hour: '2-digit',
			minute: '2-digit',
			hourCycle: 'h23',
		}).format(at);
	}
	function shiftHistoryDay(offset: number) {
		const start = historyDayBounds().start;
		if (!Number.isFinite(start.getTime())) return;
		start.setDate(start.getDate() + offset);
		historyDay = toLocalDateInput(start);
		void loadHistory();
	}
	function goToToday() {
		historyDay = toLocalDateInput(new Date());
		void loadHistory();
	}
	async function loadHistory() {
		if (!selected) return;
		historyBusy = true;
		historyError = '';
		historyRanges = [];
		try {
			if (historyJumpAt) {
				const target = new Date(historyJumpAt);
				if (Number.isFinite(target.getTime())) historyDay = toLocalDateInput(target);
				else historyJumpAt = '';
			}
			const { start, end } = historyDayBounds();
			if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime())) {
				historyError = '请选择有效日期';
				return;
			}
			const query = new URLSearchParams({ start: start.toISOString(), end: end.toISOString() });
			const result = await api<{ ranges: TimeRange[] }>(
				`/api/streams/${selected}/timeline?${query}`,
			);
			historyRanges = (result.ranges ?? []).filter((range) => range.available);
			if (historyJumpAt) {
				const at = historyJumpAt;
				historyJumpAt = '';
				const targetMs = new Date(at).getTime();
				const range = historyRanges.find(
					(item) =>
						targetMs >= new Date(item.start).getTime() &&
						targetMs < new Date(item.end).getTime(),
				);
				if (range) void playRange(range, at);
				else historyError = '指定时间没有可用录像';
			}
		} catch (e) {
			historyError = userFacingError(e);
		} finally {
			historyBusy = false;
		}
	}
	async function playRange(range: TimeRange, startAt = range.start) {
		playbackTimeMap = [];
		selectedRange = range;
		playbackStartAt = startAt;
		mode = 'dvr';
		clipStart = toPreciseLocalInput(range.start);
		clipEnd = toPreciseLocalInput(range.end);
		clipError = '';
		clipJob = undefined;
		await play();
	}
	async function startTimeShift() {
		if (!selected) return;
		const streamId = selected;
		mode = 'dvr';
		selectedRange = undefined;
		clipError = '';
		try {
			const response = await fetch(`/api/playback/dvr/${encodeURIComponent(streamId)}.m3u8`, {
				credentials: 'same-origin',
			});
			if (!response.ok) throw new Error(`无法读取时移录像 (${response.status})`);
			const playlist = await response.text();
			let segmentStart: number | undefined;
			let rangeStart: number | undefined;
			let rangeEnd: number | undefined;
			let mediaPosition = 0;
			const timeMap: typeof playbackTimeMap = [];
			for (const line of playlist.split(/\r?\n/)) {
				if (line.startsWith('#EXT-X-PROGRAM-DATE-TIME:')) {
					const value = Date.parse(line.slice('#EXT-X-PROGRAM-DATE-TIME:'.length));
					segmentStart = Number.isFinite(value) ? value : undefined;
				} else if (line.startsWith('#EXTINF:') && segmentStart !== undefined) {
					const duration = Number.parseFloat(line.slice('#EXTINF:'.length));
					if (Number.isFinite(duration) && duration > 0) {
						rangeStart ??= segmentStart;
						rangeEnd = segmentStart + duration * 1000;
						timeMap.push({
							mediaStart: mediaPosition,
							mediaEnd: mediaPosition + duration,
							wallStart: segmentStart,
						});
						mediaPosition += duration;
						segmentStart = rangeEnd;
					}
				}
			}
			if (selected !== streamId || mode !== 'dvr') return;
			if (rangeStart === undefined || rangeEnd === undefined || rangeEnd <= rangeStart) {
				throw new Error('当前没有可用于时移剪辑的录像');
			}
			const range: TimeRange = {
				start: new Date(rangeStart).toISOString(),
				end: new Date(rangeEnd).toISOString(),
				available: true,
			};
			selectedRange = range;
			playbackTimeMap = timeMap;
			clipStart = toPreciseLocalInput(range.start);
			clipEnd = toPreciseLocalInput(range.end);
			clipJob = undefined;
			await play();
		} catch (e) {
			if (selected === streamId && mode === 'dvr') {
				selectedRange = undefined;
				mode = 'live';
				error = userFacingError(e);
			}
		}
	}
	function historyPosition(range: TimeRange) {
		const { start, end } = historyDisplayBounds();
		const startMs = start.getTime();
		const endMs = end.getTime();
		const rangeStart = Math.max(startMs, Math.min(endMs, new Date(range.start).getTime()));
		return Math.max(
			0,
			Math.min(100, ((rangeStart - startMs) / Math.max(1, endMs - startMs)) * 100),
		);
	}
	function historyWidth(range: TimeRange) {
		const { start, end } = historyDisplayBounds();
		const startMs = start.getTime();
		const endMs = end.getTime();
		const rangeStart = Math.max(startMs, Math.min(endMs, new Date(range.start).getTime()));
		const rangeEnd = Math.max(startMs, Math.min(endMs, new Date(range.end).getTime()));
		return Math.max(0.35, ((rangeEnd - rangeStart) / Math.max(1, endMs - startMs)) * 100);
	}
	function playbackWallTime() {
		if (!selectedRange || !video) return '';
		const programDateTime = player?.currentProgramDateTime;
		if (programDateTime instanceof Date && Number.isFinite(programDateTime.getTime())) {
			return programDateTime.toISOString();
		}
		const mediaPosition = video.currentTime - timelineStart - playbackOffset;
		const mappedSegment = playbackTimeMap.find(
			(segment) => mediaPosition >= segment.mediaStart && mediaPosition <= segment.mediaEnd,
		);
		if (mappedSegment) {
			return new Date(
				mappedSegment.wallStart + (mediaPosition - mappedSegment.mediaStart) * 1000,
			).toISOString();
		}
		return new Date(
			new Date(selectedRange.start).getTime() +
				(video.currentTime - timelineStart - playbackOffset) * 1000,
		).toISOString();
	}
	function clipPercent(value: string) {
		if (!selectedRange || timelineEnd <= timelineStart) return 0;
		const start = new Date(selectedRange.start).getTime();
		const at = new Date(value).getTime();
		if (!Number.isFinite(start) || !Number.isFinite(at)) return 0;
		let mediaPosition = timelineStart + playbackOffset + (at - start) / 1000;
		if (mode === 'dvr' && playbackTimeMap.length) {
			const mappedSegment = playbackTimeMap.find((segment) => {
				const wallEnd = segment.wallStart + (segment.mediaEnd - segment.mediaStart) * 1000;
				return at >= segment.wallStart && at <= wallEnd;
			});
			if (!mappedSegment) return 0;
			mediaPosition =
				timelineStart +
				playbackOffset +
				mappedSegment.mediaStart +
				(at - mappedSegment.wallStart) / 1000;
		}
		return Math.max(
			0,
			Math.min(100, ((mediaPosition - timelineStart) / (timelineEnd - timelineStart)) * 100),
		);
	}
	function timelineStyle() {
		const progress =
			timelineEnd > timelineStart
				? Math.max(
						0,
						Math.min(100, ((currentTime - timelineStart) / (timelineEnd - timelineStart)) * 100),
					)
				: 0;
		const markerColor = selectedRange && mode === 'dvr' ? '#5e4bb4' : 'transparent';
		return `--progress:${progress}%;--clip-start:${clipPercent(clipStart)}%;--clip-end:${clipPercent(clipEnd)}%;--clip-marker-color:${markerColor}`;
	}
	function clipRangeReady() {
		if (!selectedRange) return false;
		const start = new Date(clipStart);
		const end = new Date(clipEnd);
		return (
			Number.isFinite(start.getTime()) &&
			Number.isFinite(end.getTime()) &&
			end > start &&
			start >= new Date(selectedRange.start) &&
			end <= new Date(selectedRange.end)
		);
	}
	function resetClipJob() {
		clipJob = undefined;
		clipError = '';
		favoriteAfterExport = false;
	}
	function markClipStart() {
		const value = playbackWallTime();
		if (value) {
			clipStart = toPreciseLocalInput(value);
			resetClipJob();
		}
	}
	function markClipEnd() {
		const value = playbackWallTime();
		if (value) {
			clipEnd = toPreciseLocalInput(value);
			resetClipJob();
		}
	}
	function generatedClipName() {
		const start = new Date(clipStart);
		return `${selectedStream?.name ?? '片段'} · ${Number.isFinite(start.getTime()) ? start.toLocaleString() : '直播片段'}`;
	}
	function openClipSettingsDialog(favorite: boolean) {
		if (!clipRangeReady()) return;
		clipDialogFavorite = favorite;
		favoriteName = '';
		clipSettingsDialogOpen = true;
	}
	async function submitClipSettings() {
		clipSettingsDialogOpen = false;
		await createHistoryClip(clipDialogFavorite, favoriteName);
	}
	async function createHistoryClip(favorite: boolean, name = '') {
		if (!selectedRange || !selected) return;
		const start = new Date(clipStart);
		const end = new Date(clipEnd);
		if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime())) {
			clipError = '请选择有效的剪辑起止时间';
			return;
		}
		if (end <= start) {
			clipError = '结束时间必须晚于开始时间';
			return;
		}
		if (start < new Date(selectedRange.start) || end > new Date(selectedRange.end)) {
			clipError = '剪辑范围必须在当前直播历史区间内';
			return;
		}
		const startTime = start.toISOString();
		const endTime = end.toISOString();
		clipBusy = true;
		clipError = '';
		favoriteAfterExport = favorite;
		try {
			clipJob = await api<ClipJob>('/api/clips', {
				method: 'POST',
				body: JSON.stringify({
					name: name.trim() || generatedClipName(),
					streamId: selected,
					startTime,
					endTime,
					mode: clipEncodingMode,
					qualityPreset: clipQualityPreset,
					resolution: clipResolution,
					gapPolicy: !historyTab && mode === 'dvr' ? 'skip' : 'reject',
					saveToLibrary: favorite,
				}),
			});
			void refreshHistoryClip();
		} catch (e) {
			clipError = userFacingError(e);
		} finally {
			clipBusy = false;
		}
	}
	async function refreshHistoryClip() {
		if (!clipJob || clipRefreshBusy) return;
		window.clearTimeout(clipRefreshTimer);
		clipRefreshTimer = 0;
		clipRefreshBusy = true;
		try {
			const job = await api<ClipJob>(`/api/clips/${clipJob.id}`);
			clipJob = job;
			if (job.status === 'COMPLETED' && favoriteAfterExport && !job.pinned) {
				await api(`/api/clips/${job.id}/pin`, {
					method: 'PUT',
					body: JSON.stringify({ pinned: true }),
				});
				favoriteAfterExport = false;
				clipJob = await api<ClipJob>(`/api/clips/${job.id}`);
			}
		} catch (e) {
			clipError = userFacingError(e);
		} finally {
			clipRefreshBusy = false;
			if (
				clipJob &&
				['QUEUED', 'RESOLVING', 'RESERVED', 'RUNNING', 'VERIFYING'].includes(clipJob.status)
			) {
				clipRefreshTimer = window.setTimeout(() => void refreshHistoryClip(), 1500);
			}
		}
	}
	function formatTime(value: number) {
		const total = Math.max(0, Math.floor(value));
		const hours = Math.floor(total / 3600);
		const minutes = Math.floor((total % 3600) / 60);
		const seconds = total % 60;
		return hours
			? `${hours}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
			: `${minutes}:${String(seconds).padStart(2, '0')}`;
	}
	onMount(() => {
		const nativeVideo = video as HTMLVideoElement & { webkitEnterFullscreen?: () => void };
		const onNativeBegin = () => {
			nativeVideoFullscreen = true;
			handleFullscreenChange();
		};
		const onNativeEnd = () => {
			nativeVideoFullscreen = false;
			handleFullscreenChange();
		};
		const onOpenPushDialog = () => openPushDialog();
		const disconnect = connectDvrEvents(
			{
				'stream.started': () => {
					void refresh();
					void loadHistory();
				},
				'stream.stopped': () => {
					void refresh();
					void loadHistory();
				},
				'stream.deleted': () => void refresh(),
				'timeline.updated': () => void loadHistory(),
				'clip.progress': () => void refreshHistoryClip(),
				'clip.completed': () => void refreshHistoryClip(),
			},
			(auth) => {
				role = auth.role ?? '';
				if (auth.authenticated) void refresh();
				else {
					streams = [];
					active = {};
					status = '请先登录后查看频道直播';
				}
			},
		);
		liveStatusTimer = window.setInterval(() => void refreshLiveStatus(), 5000);
		window.addEventListener('dvr-open-push-dialog', onOpenPushDialog);
		window.addEventListener('keydown', handleKeyboardShortcuts);
		document.addEventListener('fullscreenchange', handleFullscreenChange);
		video?.addEventListener('webkitbeginfullscreen', onNativeBegin);
		video?.addEventListener('webkitendfullscreen', onNativeEnd);
		return () => {
			window.clearTimeout(clipRefreshTimer);
			window.clearTimeout(controlsHideTimer);
			stopLivePosterPreview();
			window.clearInterval(debugTimer);
			window.clearInterval(liveStatusTimer);
			window.removeEventListener('dvr-open-push-dialog', onOpenPushDialog);
			disconnect();
			window.removeEventListener('keydown', handleKeyboardShortcuts);
			document.removeEventListener('fullscreenchange', handleFullscreenChange);
			video?.removeEventListener('webkitbeginfullscreen', onNativeBegin);
			video?.removeEventListener('webkitendfullscreen', onNativeEnd);
			video?.pause();
			player?.destroy();
		};
	});
</script>

<section class="page-head">
	<div class="channel-heading">
		<h1>{historyTab ? '直播历史' : (selectedStream?.name ?? '频道详情')}</h1>
		<span>{historyTab ? '按日期浏览、回放和剪辑已录制内容' : '实时直播 · 时移回看'}</span>
	</div>
	<div class="head-actions">
		{#if historyTab}
			<button class="button secondary" onclick={loadHistory}>
				{historyBusy ? '载入中…' : '刷新历史'}
			</button>
		{:else if browserPublish.active}<button class="button danger" onclick={stopBrowserPublish}>
				停止{browserPublish.label}推流
			</button>{/if}
		{#if !historyTab}<button class="button secondary" onclick={refresh}>刷新状态</button>{/if}
	</div>
</section>
{#if !historyTab && selected && streamInterrupted}
	<div class="stream-interrupted-notice" role="status" aria-live="polite">
		<span class="interrupted-icon" aria-hidden="true">!</span>
		<div>
			<strong>直播推流已中断</strong>
			<p>频道信号已断开，画面停止更新。推流恢复后会自动继续。</p>
		</div>
		<button class="button secondary" onclick={refresh}>检查状态</button>
	</div>
{/if}
<div class="player-layout">
	<section
		class="panel video-panel"
		class:web-fullscreen={webFullscreen}
		class:controls-hidden={isPlaying && (fullscreenActive || webFullscreen) && !controlsVisible}
		bind:this={videoPanel}
		aria-label="视频播放器"
		onpointermove={revealPlayerControls}
		onpointerdown={handlePlayerPointerDown}
		oncontextmenu={handlePlayerContextMenu}
	>
		<div class="video-top" class:playback-only={historyTab}>
			{#if historyTab}
				<div class="live-pill replay-pill">
					<i></i>
					{selectedRange ? '历史回放' : '选择录像'}
				</div>
				<div class="channel-name">{selectedStream?.name ?? '频道'}</div>
				{#if selectedRange}<span class="replay-time">
						{new Date(selectedRange.start).toLocaleString()} — {new Date(
							selectedRange.end,
						).toLocaleTimeString()}
					</span>{/if}
			{:else}
				<div class="live-pill">
					<i></i>
					{mode === 'live'
						? active[selected]
							? '直播中'
							: streamInterrupted
								? '推流中断'
								: '等待推流'
						: '时移回看'}
				</div>
				<div class="channel-name">{selectedStream?.name ?? '频道'}</div>
				<div class="spacer"></div>
				<button
					class:chosen={mode === 'live'}
					class="mode"
					onclick={() => {
						mode = 'live';
						selectedRange = undefined;
						void play();
					}}
				>
					直播
				</button>
				<button class:chosen={mode === 'dvr'} class="mode" onclick={() => void startTimeShift()}>
					时移
				</button>
			{/if}
		</div>
		<div
			class="screen"
			bind:this={screenPanel}
		>
			<video
				bind:this={video}
				poster={!historyTab ? livePoster || undefined : undefined}
				muted={mutedPlayback}
				playsinline
				ontimeupdate={updateTimeline}
				onprogress={updateTimeline}
				onloadedmetadata={updateTimeline}
				onseeking={updateTimeline}
				onplay={handleVideoPlay}
				onpause={handleVideoPause}
				onerror={handleMediaError}
				onclick={handleScreenClick}
				ondblclick={handleVideoDoubleClick}
			></video>
			{#if !selected || (historyTab ? !selectedRange : mode === 'live' && !active[selected])}<div
					class="empty-screen"
				>
					<div class="play-icon">▶</div>
					<span>
						{historyTab
							? '选择一段录像开始回放'
							: selected
								? streamInterrupted
									? '推流已中断，等待信号恢复'
									: '频道尚未开播'
								: '选择频道并开始播放'}
					</span>
					{#if selected && !historyTab}<small>
							{streamInterrupted ? '推流恢复后可点击播放' : '开始推流后可点击播放'}
						</small>{/if}
				</div>{/if}
			{#if !historyTab && mode === 'live' && active[selected] && !livePlaybackStarted}
				<button
					class="live-poster-play"
					aria-label="播放直播并开启声音"
					onclick={() => void play()}
				>
					<span class="poster-play-icon" aria-hidden="true">▶</span>
					<span>{livePosterLoading ? '正在载入直播画面…' : '点击播放直播'}</span>
				</button>
			{/if}
			{#if debugOpen}<aside class="debug-overlay" aria-label="播放器统计信息">
					<div class="debug-overlay-head">
						<div>
							<strong>统计信息</strong>
							<span>Stats for nerds · D 关闭</span>
						</div>
						<button type="button" aria-label="关闭统计信息" onclick={closeDebugStats}>×</button>
					</div>
					<dl>
						<div>
							<dt>频道 / 状态</dt>
							<dd>{(selectedStream?.name ?? selected) || '—'} · {debugStats.state}</dd>
						</div>
						<div>
							<dt>播放方式</dt>
							<dd>{playbackEngine}</dd>
						</div>
						<div>
							<dt>实际分辨率</dt>
							<dd>{debugStats.resolution}</dd>
						</div>
						<div>
							<dt>HLS 清晰度 / 码率</dt>
							<dd>{debugStats.level}</dd>
						</div>
						<div>
							<dt>估算带宽</dt>
							<dd>{debugStats.bandwidth}</dd>
						</div>
						<div>
							<dt>缓冲长度</dt>
							<dd>{debugStats.buffer}</dd>
						</div>
						<div>
							<dt>直播延迟</dt>
							<dd>{debugStats.latency}</dd>
						</div>
						<div>
							<dt>FPS</dt>
							<dd>{debugStats.fps}</dd>
						</div>
						<div>
							<dt>掉帧 / 总帧数</dt>
							<dd>{debugStats.frames}</dd>
						</div>
						<div>
							<dt>最近错误</dt>
							<dd class="debug-error">{debugLastError}</dd>
						</div>
					</dl>
				</aside>{/if}
		</div>
		{#if selected}<div
				class="custom-controls"
				aria-label="播放器控制：空格或 K 播放暂停，方向键跳转，M 声音，F 全屏"
			>
				<button
					class="control-button icon-button"
					aria-label={!historyTab && mode === 'live' && !active[selected]
						? '等待推流'
						: isPlaying
							? '暂停'
							: '播放'}
					title={!historyTab && mode === 'live' && !active[selected]
						? '等待推流'
						: `${isPlaying ? '暂停' : '播放'}（空格 / K）`}
					disabled={historyTab ? !selectedRange : mode === 'live' && !active[selected]}
					onclick={togglePlayback}
				>
					{!historyTab && mode === 'live' && !active[selected] ? '▶' : isPlaying ? 'Ⅱ' : '▶'}
				</button>
				{#if historyTab}<button
						class="control-button skip-button"
						aria-label="后退 15 秒"
						title="后退 15 秒"
						disabled={!selectedRange || timelineEnd <= timelineStart}
						onclick={() => skipPlayback(-15)}
					>
						<svg viewBox="0 0 24 24" aria-hidden="true">
							<path d="M3 8V3h5" />
							<path d="M4 7a9 9 0 1 1-1 5" />
							<text x="12" y="14.5" text-anchor="middle">15s</text>
						</svg>
					</button><button
						class="control-button skip-button"
						aria-label="前进 15 秒"
						title="前进 15 秒"
						disabled={!selectedRange || timelineEnd <= timelineStart}
						onclick={() => skipPlayback(15)}
					>
						<svg viewBox="0 0 24 24" aria-hidden="true">
							<path d="M21 8V3h-5" />
							<path d="M20 7a9 9 0 1 0 1 5" />
							<text x="12" y="14.5" text-anchor="middle">15s</text>
						</svg>
					</button>{/if}
				{#if historyTab || mode === 'dvr'}<span class="time-label">
						{formatTime(currentTime - timelineStart)}
					</span>{/if}
				<input
					class="timeline"
					aria-label="播放进度"
					title="←/→ 快退或快进 5 秒，Shift 加 10 秒"
					type="range"
					disabled={historyTab ? !selectedRange : mode === 'live' && !active[selected]}
					min={timelineStart}
					max={Math.max(timelineEnd, timelineStart + 0.1)}
					step="0.1"
					value={Math.min(
						Math.max(currentTime, timelineStart),
						Math.max(timelineEnd, timelineStart + 0.1),
					)}
					style={timelineStyle()}
					oninput={(event) => seekTo(Number(event.currentTarget.value))}
				/>
				{#if historyTab || mode === 'dvr'}<span class="time-label">
						{formatTime(timelineEnd - timelineStart)}
					</span>{:else if active[selected]}<button
						class:at-live={atLiveEdge}
						class="live-edge"
					aria-label={atLiveEdge ? '正在直播' : '回到直播'}
					title={atLiveEdge ? '正在直播' : '回到直播'}
						onclick={goLive}
					>
						<i></i>
					{atLiveEdge ? '直播中' : '回到直播'}
					</button>{:else}<span class="live-edge waiting">
						<i></i>
						等待推流
					</span>{/if}
				<button
					class="control-button icon-button"
					aria-label={mutedPlayback ? '开启声音' : '静音'}
					title={mutedPlayback ? '开启声音（M）' : '静音（M）'}
					onclick={toggleMute}
				>
					<svg viewBox="0 0 24 24" aria-hidden="true">
						<path d="M11 5 6 9H3v6h3l5 4V5Z" />
						<path
							d={mutedPlayback
								? 'm16 9 5 6m0-6-5 6'
								: 'M15.5 8.5a5 5 0 0 1 0 7m3-10a9 9 0 0 1 0 13'}
						/>
					</svg>
				</button>
				<button
					class="control-button icon-button"
					aria-label={fullscreenActive || webFullscreen ? '退出全屏' : '全屏'}
					title={fullscreenActive || webFullscreen ? '退出全屏（F）' : '全屏（F）'}
					onclick={toggleFullscreen}
				>
					<svg viewBox="0 0 24 24" aria-hidden="true">
						{#if fullscreenActive || webFullscreen}<path
								d="M8 4v4H4m12-4v4h4M8 20v-4H4m12 4v-4h4"
							/>
							<path d="m4 8 4-4m8 0 4 4M4 16l4 4m8 0 4-4" />{:else}<path
								d="M3 8V3h5m8 0h5v5M3 16v5h5m8 0h5v-5"
							/>
							<path d="m3 8 5-5m8 0 5 5M3 16l5 5m8 0 5-5" />{/if}
					</svg>
				</button>
			</div>{/if}
		{#if historyTab}<div class="player-foot replay-foot">
				<span class="state-dot"></span>
				{selectedRange ? (isPlaying ? '正在回放' : '回放已暂停') : '等待选择录像'}
				<span class="spacer"></span>
				{#if selectedRange}<span>{new Date(selectedRange.start).toLocaleString()}</span>{/if}
			</div>{:else}<div class="player-foot">
				<span class="state-dot"></span>
				{status}
			</div>{/if}
		{#if error || (!historyTab && browserPublish.error)}<p class="error">
				{error || browserPublish.error}
			</p>{/if}
		{#if debugMenuOpen}<div
				class="player-context-menu"
				bind:this={debugMenu}
				style={`left:${debugMenuX}px;top:${debugMenuY}px`}
				role="menu"
			>
				<button
					type="button"
					role="menuitem"
					onclick={() => {
						debugMenuOpen = false;
						toggleDebugStats();
					}}
				>
					{debugOpen ? '隐藏统计信息' : '显示统计信息'}
					<kbd>D</kbd>
				</button>
			</div>{/if}
	</section>
</div>
{#if historyTab || mode === 'dvr'}<section
		class="panel history-clip-panel"
		aria-labelledby="history-clip-title"
	>
		<div class="history-clip-head">
			<div>
				<div class="eyebrow">{historyTab ? '回放剪辑' : '时移剪辑'}</div>
				<h2 id="history-clip-title">剪辑操作</h2>
			</div>
			<div class="history-clip-summary">
				<strong>
					{selectedRange ? '当前剪辑区间' : historyTab ? '尚未选择录像' : '尚未选择区间'}
				</strong>
				<span>
					{#if selectedRange}
						{new Date(clipStart).toLocaleString()} — {new Date(clipEnd).toLocaleTimeString()}
					{:else}
						{historyTab ? '请在下方直播历史中选择一段录像' : '播放时移内容并标记剪辑起止点'}
					{/if}
				</span>
			</div>
		</div>
		<div class="clip-toolbar-actions" aria-label={historyTab ? '历史录像剪辑操作' : '时移剪辑操作'}>
			<button
				class="button secondary"
				disabled={!selectedRange || mode !== 'dvr'}
				onclick={markClipStart}
			>
				设为起点
			</button>
			<button
				class="button secondary"
				disabled={!selectedRange || mode !== 'dvr'}
				onclick={markClipEnd}
			>
				设为终点
			</button>
			{#if clipJob?.status === 'COMPLETED' && !clipJob.request.saveToLibrary}
				<a
					class="button"
					href={`/api/clips/${clipJob.id}/download`}
					download={`clip-${clipJob.id}.mp4`}
				>
					{historyTab ? '下载 MP4' : '下载片段'}
				</a>
			{:else}
				<button
					class="button"
					disabled={!clipRangeReady() ||
						clipBusy ||
						clipJob?.status === 'QUEUED' ||
						clipJob?.status === 'RUNNING'}
					onclick={() => openClipSettingsDialog(false)}
				>
					{clipJob?.status === 'RUNNING' || clipJob?.status === 'QUEUED'
						? `导出中 ${Math.round(clipJob.progress * 100)}%`
						: historyTab
							? '导出片段'
							: '下载'}
				</button>
			{/if}
			<button
				class="button secondary"
				disabled={!clipRangeReady() ||
					clipBusy ||
					clipJob?.status === 'QUEUED' ||
					clipJob?.status === 'RUNNING' ||
					Boolean(clipJob?.status === 'COMPLETED' && clipJob.pinned)}
				onclick={() => openClipSettingsDialog(true)}
			>
				{clipJob?.status === 'COMPLETED' && clipJob.pinned
					? '已收藏'
					: historyTab
						? '收藏片段'
						: '收藏'}
			</button>
		</div>
		{#if clipError}<p class="error">{clipError}</p>{/if}
		{#if clipJob}<small class="clip-job-state">
				{clipStatusLabel(clipJob.status)} · {Math.round(clipJob.progress * 100)}%
			</small>{/if}
	</section>{/if}
{#if historyTab}<section class="panel history-panel">
		<div class="history-head">
			<div>
				<div class="eyebrow">录像档案</div>
				<h2>直播历史</h2>
				<p>按日期浏览录像，选择一段即可回放、剪辑或收藏。</p>
			</div>
			<div class="history-filter">
				<button
					class="button secondary day-nav"
					aria-label="前一天"
					title="前一天"
					disabled={historyBusy}
					onclick={() => shiftHistoryDay(-1)}
				>
					‹
				</button>
				<label>
					日期
					<input
						type="date"
						bind:value={historyDay}
						max={toLocalDateInput(new Date())}
						onchange={() => void loadHistory()}
					/>
				</label>
				<button
					class="button secondary day-nav"
					aria-label="后一天"
					title="后一天"
					disabled={historyBusy || historyDay >= toLocalDateInput(new Date())}
					onclick={() => shiftHistoryDay(1)}
				>
					›
				</button>
				<button class="button secondary" disabled={historyBusy || !selected} onclick={goToToday}>
					今天
				</button>
			</div>
		</div>
		{#if historyError}<p class="error">{historyError}</p>{/if}
		{#if historyRanges.length}<div class="history-visual">
				<div class="history-ticks">
					{#each [0, 0.25, 0.5, 0.75, 1] as position}
						<span style={`left:${position * 100}%`}>{historyTickLabel(position)}</span>
					{/each}
				</div>
				<div class="history-track">
					{#each historyRanges as range}<button
							class:active-range={selectedRange?.start === range.start}
							class="history-segment"
							style={`left:${historyPosition(range)}%;width:${historyWidth(range)}%`}
							title={`${new Date(range.start).toLocaleString()} — ${new Date(range.end).toLocaleString()}`}
							aria-label="回放这段直播历史"
							onclick={() => void playRange(range)}
						></button>{/each}
				</div>
				<div class="history-legend">
					<i></i>
					已录制 · 点击区间回放
				</div>
			</div>
			<div class="history-list">
				{#each historyRangesNewestFirst as range}<button
						class:active-history={selectedRange?.start === range.start}
						class="history-row"
						onclick={() => void playRange(range)}
					>
						<span class="history-play">▶</span>
						<span class="history-copy">
							<strong>
								{new Date(range.start).toLocaleString()} — {new Date(
									range.end,
								).toLocaleTimeString()}
							</strong>
							<small>
								{range.sessionId ? `直播场次 ${range.sessionId}` : '已录制'} · {Math.max(
									1,
									Math.round(
										(new Date(range.end).getTime() - new Date(range.start).getTime()) / 60000,
									),
								)} 分钟
							</small>
						</span>
						<span class="history-open">
							{selectedRange?.start === range.start ? '正在回放' : '查看回放 →'}
						</span>
					</button>{/each}
			</div>{:else if !historyBusy}<div class="history-empty">
				所选日期没有直播录像，可切换日期查看其他场次。
			</div>{/if}
	</section>{/if}

{#if clipSettingsDialogOpen}<div class="modal-backdrop">
		<div
			class="modal"
			role="dialog"
			aria-modal="true"
			aria-labelledby="clip-settings-modal-title"
			tabindex="-1"
		>
			<div class="eyebrow">片段设置</div>
			<h2 id="clip-settings-modal-title">{clipDialogFavorite ? '收藏片段' : '导出片段'}</h2>
			<p>选择导出方式；快速模式不重新编码，精确模式可以设置分辨率和画质。</p>
			{#if clipDialogFavorite}<label>
				片段名称
				<input bind:value={favoriteName} maxlength="100" placeholder={generatedClipName()} />
			</label>{/if}
			<label>
				导出模式
				<select bind:value={clipEncodingMode}>
					<option value="fast">快速（不重新编码，边界可能贴近关键帧）</option>
					<option value="accurate">精确（重新编码）</option>
				</select>
			</label>
			{#if clipEncodingMode === 'accurate'}<label>
				输出分辨率
				<select bind:value={clipResolution}>
					<option value="source">保持源分辨率</option>
					<option value="1080p">1080p</option>
					<option value="720p">720p（默认）</option>
					<option value="480p">480p</option>
				</select>
			</label><label>
				画质预设
				<select bind:value={clipQualityPreset}>
					<option value="quality">画质优先（CRF 18，medium）</option>
					<option value="balanced">均衡（CRF 23，veryfast）</option>
					<option value="compact">体积优先（CRF 27，veryfast）</option>
				</select>
			</label>{/if}
			<div class="modal-actions">
				<button class="button secondary" onclick={() => (clipSettingsDialogOpen = false)}>取消</button>
				<button class="button" disabled={clipBusy} onclick={submitClipSettings}>
					{clipBusy ? '正在提交…' : clipDialogFavorite ? '确认收藏' : '开始导出'}
				</button>
			</div>
		</div>
	</div>{/if}
{#if pushDialogOpen}<div class="modal-backdrop">
		<section class="modal" aria-labelledby="push-modal-title">
			<div class="eyebrow">开始直播</div>
			{#if pushDialogView === 'options'}<h2 id="push-modal-title">选择推流方式</h2>
				<p>浏览器可直接采集屏幕或摄像头；OBS 使用 RTMP 推流码连接。</p>
				<div class="source-choice">
					<button onclick={() => void startBrowserPush()} disabled={pushKeyBusy}>
						<strong>▣ 浏览器推流</strong>
						<small>直接共享屏幕或摄像头，无需配置 OBS</small>
					</button>
					<button onclick={() => void showObsPushInfo()} disabled={pushKeyBusy}>
						<strong>◉ OBS 推流</strong>
						<small>查看 RTMP 地址和频道推流码</small>
					</button>
				</div>
				{#if pushKeyBusy}<p class="secure-note">正在读取频道推流信息…</p>{/if}{#if pushKeyError}<p
						class="error"
					>
						{pushKeyError}
					</p>{/if}
				<div class="modal-actions">
					<button class="button secondary" onclick={() => (pushDialogOpen = false)}>关闭</button>
				</div>{:else}<button
					class="back-link"
					onclick={() => {
						pushDialogView = 'options';
						pushKeyError = '';
					}}
				>
					← 推流方式
				</button>
				<h2 id="push-modal-title">OBS 推流</h2>
				<p>在 OBS 的“设置 → 推流”中选择自定义服务并填写以下信息。</p>
				{#if pushKeyBusy}<p class="secure-note">正在读取频道推流码…</p>{:else if pushKeyInfo}<label>
						服务器地址
						<div class="copy-field">
							<input readonly value={`rtmp://${window.location.hostname}:1935/live`} />
							<button
								class="button secondary"
								onclick={() =>
									void copyPushField(`rtmp://${window.location.hostname}:1935/live`, 'server')}
							>
								{copiedPushField === 'server' ? '已复制' : '复制'}
							</button>
						</div>
					</label>
					<label>
						推流码
						<div class="copy-field">
							<input readonly value={pushKeyInfo.obs.streamKey} />
							<button
								class="button secondary"
								onclick={() => void copyPushField(pushKeyInfo!.obs.streamKey, 'key')}
							>
								{copiedPushField === 'key' ? '已复制' : '复制'}
							</button>
						</div>
					</label>
					{#if pushKeyInfo.rotated}<p class="secure-note">
							此频道使用旧版密钥存储，原密钥无法恢复；已生成新推流码，旧码已失效。
						</p>{/if}{/if}{#if pushKeyError}<p class="error">{pushKeyError}</p>{/if}
				<div class="modal-actions">
					<button class="button secondary" onclick={() => (pushDialogOpen = false)}>完成</button>
				</div>{/if}
		</section>
	</div>{/if}
{#if browserPushOpen}<div class="modal-backdrop">
		<section class="modal">
			<div class="eyebrow">浏览器推流 · Chrome</div>
			<h2>浏览器推流</h2>
			<p>选择屏幕或摄像头，浏览器会直接将画面和声音推送到当前频道。</p>
			<div class="source-choice">
				<button
					class:chosen={browserPushMode === 'screen'}
					onclick={() => (browserPushMode = 'screen')}
				>
					<strong>▣ 屏幕</strong>
					<small>共享窗口、标签页或整个屏幕</small>
				</button>
				<button
					class:chosen={browserPushMode === 'camera'}
					onclick={() => (browserPushMode = 'camera')}
				>
					<strong>◉ 摄像头</strong>
					<small>使用摄像头和麦克风</small>
				</button>
			</div>
			<label class="frame-rate-setting">
				视频帧率
				<select aria-label="视频帧率" bind:value={browserPushFrameRate}>
					<option value={24}>24 FPS</option>
					<option value={30}>30 FPS</option>
					<option value={60}>60 FPS</option>
				</select>
			</label>
			<label class="frame-rate-setting">
				分辨率上限
				<select aria-label="分辨率上限" bind:value={browserPushResolution}>
					<option value="1080p">1080p（推荐）</option>
					<option value="720p">720p</option>
					<option value="original">原始分辨率</option>
				</select>
			</label>
			{#if browserPublish.error}<p class="error">{browserPublish.error}</p>{/if}
			<p class="secure-note">
				分辨率上限会在采集时缩放画面，不改变共享来源。实际帧率取决于设备和浏览器。
			</p>
			<p class="secure-note">需要 HTTPS（localhost 除外）；采集权限只在推流期间使用。</p>
			<div class="modal-actions">
				<button
					class="button secondary"
					disabled={browserPublish.busy}
					onclick={() => (browserPushOpen = false)}
				>
					取消
				</button>
				<button
					class="button"
					disabled={browserPublish.busy || !browserPushKey || !selected}
					onclick={startBrowserPublish}
				>
					{browserPublish.busy ? '正在连接…' : '开始推流'}
				</button>
			</div>
		</section>
	</div>{/if}

<style>
	.page-head {
		width: min(100%, 80vw);
		margin-left: auto;
		margin-right: auto;
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 16px;
		margin: -14px 0 14px;
		min-height: 42px;
	}
	.stream-interrupted-notice {
		width: min(100%, 80vw);
		margin: 0 auto 12px;
		padding: 12px 14px;
		border: 1px solid #f0c6a5;
		border-radius: 10px;
		background: #fff8f1;
		color: #844b1c;
		display: flex;
		align-items: center;
		gap: 11px;
	}
	.stream-interrupted-notice > div {
		flex: 1;
		min-width: 0;
	}
	.stream-interrupted-notice strong {
		display: block;
		font-size: 12px;
	}
	.stream-interrupted-notice p {
		margin-top: 3px;
		color: #966b47;
	}
	.interrupted-icon {
		width: 24px;
		height: 24px;
		flex: none;
		border-radius: 50%;
		background: #f8e3d2;
		color: #ad5c20;
		font-weight: 800;
		line-height: 24px;
		text-align: center;
	}
	.channel-heading {
		display: flex;
		align-items: baseline;
		gap: 12px;
		min-width: 0;
	}
	.channel-heading h1 {
		font-size: 23px;
		line-height: 1.2;
		letter-spacing: -0.5px;
		margin: 0;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.channel-heading span {
		color: #758194;
		font-size: 11px;
		white-space: nowrap;
	}
	h1 {
		font-size: 30px;
		line-height: 1.2;
		letter-spacing: -0.7px;
		margin: 6px 0;
	}
	h2 {
		font-size: 16px;
		color: #263143;
		margin: 4px 0;
	}
	p {
		color: #758194;
		margin: 0;
		font-size: 12px;
	}
	.head-actions {
		display: flex;
		align-items: center;
		gap: 8px;
		flex-wrap: wrap;
		flex: none;
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
	.button.danger {
		background: #fff0f0;
		color: #b73342;
		border-color: #f2cdd1;
	}
	.button:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.player-layout {
		display: grid;
		grid-template-columns: minmax(0, 1fr);
	}
	.panel {
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
		padding: 16px;
		margin-bottom: 16px;
	}
	.video-panel {
		position: relative;
		width: min(100%, 80vw);
		margin-inline: auto;
		padding: 14px;
	}
	.debug-overlay {
		position: absolute;
		z-index: 5;
		top: 12px;
		left: 12px;
		width: min(390px, calc(100% - 24px));
		max-height: calc(100% - 24px);
		overflow: auto;
		padding: 12px 14px;
		border: 1px solid #ffffff24;
		border-radius: 9px;
		background: #080b12e8;
		box-shadow: 0 8px 30px #0008;
		color: #e7ebf3;
		font:
			11px/1.4 ui-monospace,
			SFMono-Regular,
			Menlo,
			Consolas,
			monospace;
		backdrop-filter: blur(10px);
	}
	.debug-overlay-head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 12px;
		margin-bottom: 9px;
	}
	.debug-overlay-head > div {
		display: grid;
		gap: 2px;
	}
	.debug-overlay-head strong {
		font-size: 12px;
	}
	.debug-overlay-head span {
		color: #9ca8ba;
		font-size: 10px;
	}
	.debug-overlay-head button {
		border: 0;
		background: transparent;
		color: #c6cfdd;
		font-size: 19px;
		line-height: 1;
		cursor: pointer;
	}
	.debug-overlay dl {
		display: grid;
		gap: 6px;
		margin: 0;
	}
	.debug-overlay dl > div {
		display: grid;
		grid-template-columns: 120px minmax(0, 1fr);
		gap: 9px;
	}
	.debug-overlay dt {
		color: #9ca8ba;
	}
	.debug-overlay dd {
		min-width: 0;
		margin: 0;
		overflow-wrap: anywhere;
	}
	.debug-overlay .debug-error {
		color: #ff9eaa;
	}
	.player-context-menu {
		position: absolute;
		z-index: 20;
		min-width: 190px;
		padding: 5px;
		border: 1px solid #303848;
		border-radius: 8px;
		background: #111620f5;
		box-shadow: 0 8px 26px #0007;
	}
	.player-context-menu button {
		width: 100%;
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 20px;
		padding: 9px 10px;
		border: 0;
		border-radius: 5px;
		background: transparent;
		color: #e7ebf3;
		font: inherit;
		text-align: left;
		cursor: pointer;
	}
	.player-context-menu button:hover {
		background: #252d3b;
	}
	.player-context-menu kbd {
		padding: 2px 5px;
		border: 1px solid #444e60;
		border-radius: 4px;
		color: #b9c3d3;
		font: inherit;
	}
	.video-top {
		display: flex;
		align-items: center;
		gap: 9px;
		padding: 0 0 12px;
	}
	.live-pill {
		font-size: 10px;
		font-weight: 700;
		color: #bf3947;
		background: #fff0f1;
		padding: 6px 9px;
		border-radius: 6px;
		display: flex;
		gap: 6px;
		align-items: center;
	}
	.live-pill i {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: #ed5864;
	}
	.video-top.playback-only {
		justify-content: flex-start;
	}
	.replay-pill {
		color: #6653c7;
		background: #f0edff;
	}
	.replay-pill i {
		background: #7561d3;
	}
	.replay-time {
		margin-left: auto;
		color: #758194;
		font-size: 11px;
		font-variant-numeric: tabular-nums;
	}
	.replay-foot .state-dot {
		background: #7561d3;
	}
	.channel-name {
		color: #344054;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 7px 10px;
		font-size: 11px;
		font-weight: 600;
	}
	.spacer {
		flex: 1;
	}
	.mode {
		background: transparent;
		color: #738095;
		border: 0;
		border-radius: 7px;
		padding: 8px 11px;
		font-size: 11px;
	}
	.mode.chosen {
		color: #5743b1;
		background: #f0edff;
	}
	.screen {
		aspect-ratio: 16/9;
		position: relative;
		background: #080a0f;
		border-radius: 8px;
		overflow: hidden;
	}
	.screen video {
		width: 100%;
		height: 100%;
		object-fit: contain;
	}
	.live-poster-play {
		position: absolute;
		inset: 0;
		z-index: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 10px;
		border: 0;
		background: linear-gradient(180deg, #080a0f12, #080a0f42);
		color: #fff;
		font: inherit;
		font-size: 12px;
		text-shadow: 0 1px 5px #000b;
		cursor: pointer;
	}
	.poster-play-icon {
		width: 54px;
		height: 54px;
		display: grid;
		place-items: center;
		padding-left: 3px;
		border: 1px solid #ffffffa8;
		border-radius: 50%;
		background: #1519229c;
		font-size: 20px;
		text-shadow: none;
		backdrop-filter: blur(6px);
	}
	.live-poster-play:hover .poster-play-icon {
		background: #6653c7e8;
	}
	.empty-screen {
		position: absolute;
		inset: 0;
		display: flex;
		flex-direction: column;
		gap: 11px;
		align-items: center;
		justify-content: center;
		color: #8791a1;
		font-size: 12px;
		pointer-events: none;
	}
	.empty-screen small {
		color: #a0a9b7;
		font-size: 11px;
	}
	.play-icon {
		width: 43px;
		height: 43px;
		border: 1px solid #343e50;
		border-radius: 50%;
		display: grid;
		place-items: center;
		color: #a18dfc;
		padding-left: 3px;
	}
	.custom-controls {
		display: flex;
		align-items: center;
		gap: 10px;
		padding: 11px 3px 1px;
		color: #667286;
	}
	.video-top,
	.custom-controls,
	.player-foot {
		overflow: hidden;
		transition:
			opacity 180ms ease,
			visibility 180ms ease,
			max-height 180ms ease,
			padding 180ms ease;
	}
	.video-top {
		max-height: 52px;
	}
	.custom-controls {
		max-height: 52px;
	}
	.player-foot {
		max-height: 38px;
	}
	.video-panel.controls-hidden .video-top,
	.video-panel.controls-hidden .custom-controls,
	.video-panel.controls-hidden .player-foot {
		opacity: 0;
		visibility: hidden;
		pointer-events: none;
		max-height: 0;
		padding-top: 0;
		padding-bottom: 0;
	}
	.control-button {
		flex: none;
		min-height: 31px;
		padding: 0 9px;
		border: 1px solid #e2e6ed;
		border-radius: 7px;
		background: #fff;
		color: #4d596b;
		font-size: 10px;
		font-weight: 600;
		cursor: pointer;
	}
	.control-button:hover {
		background: #f5f3ff;
		border-color: #d8d0ff;
		color: #5140a3;
	}
	.control-button:disabled {
		opacity: 0.45;
		cursor: not-allowed;
	}
	.icon-button {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 32px;
		padding: 0;
		font-size: 13px;
		line-height: 1;
	}
	.skip-button {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex: none;
		width: 38px;
		min-width: 38px;
		padding: 0;
	}
	.skip-button svg {
		width: 23px;
		height: 23px;
		fill: none;
		stroke: currentColor;
		stroke-width: 1.8;
		stroke-linecap: round;
		stroke-linejoin: round;
	}
	.skip-button svg text {
		fill: currentColor;
		stroke: none;
		font-size: 7.5px;
		font-weight: 700;
	}
	.time-label {
		flex: none;
		color: #748093;
		font-size: 10px;
		font-variant-numeric: tabular-nums;
	}
	.timeline {
		--progress: 0%;
		appearance: none;
		flex: 1;
		min-width: 48px;
		height: 18px;
		margin: 0;
		padding: 0;
		background: transparent;
		cursor: pointer;
		touch-action: none;
	}
	.timeline::-webkit-slider-runnable-track {
		height: 6px;
		border-radius: 999px;
		background:
			linear-gradient(
				to right,
				rgba(120, 101, 221, 0.28) 0%,
				rgba(120, 101, 221, 0.28) var(--progress),
				transparent var(--progress)
			),
			linear-gradient(
				to right,
				transparent 0%,
				transparent calc(var(--clip-start) - 1px),
				var(--clip-marker-color) calc(var(--clip-start) - 1px),
				var(--clip-marker-color) calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-end) - 1px),
				var(--clip-marker-color) calc(var(--clip-end) - 1px),
				var(--clip-marker-color) calc(var(--clip-end) + 1px),
				transparent calc(var(--clip-end) + 1px)
			),
			linear-gradient(
				to right,
				#e5e8ef 0%,
				#e5e8ef var(--clip-start),
				#b9adff var(--clip-start),
				#b9adff var(--clip-end),
				#e5e8ef var(--clip-end),
				#e5e8ef 100%
			);
		transition: height 0.14s ease;
	}
	.timeline::-webkit-slider-thumb {
		appearance: none;
		width: 15px;
		height: 15px;
		margin-top: -4.5px;
		border: 3px solid #fff;
		border-radius: 50%;
		background: #6d59d1;
		box-shadow:
			0 1px 4px #1d183d44,
			0 0 0 1px #6d59d1;
		transition:
			transform 0.14s ease,
			box-shadow 0.14s ease;
	}
	.timeline:hover::-webkit-slider-runnable-track {
		height: 8px;
	}
	.timeline:hover::-webkit-slider-thumb {
		transform: scale(1.08);
		box-shadow:
			0 1px 5px #1d183d55,
			0 0 0 2px #6d59d1;
	}
	.timeline::-moz-range-track {
		height: 6px;
		border-radius: 999px;
		background:
			linear-gradient(
				to right,
				transparent 0%,
				transparent calc(var(--clip-start) - 1px),
				var(--clip-marker-color) calc(var(--clip-start) - 1px),
				var(--clip-marker-color) calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-end) - 1px),
				var(--clip-marker-color) calc(var(--clip-end) - 1px),
				var(--clip-marker-color) calc(var(--clip-end) + 1px),
				transparent calc(var(--clip-end) + 1px)
			),
			linear-gradient(
				to right,
				#e5e8ef 0%,
				#e5e8ef var(--clip-start),
				#b9adff var(--clip-start),
				#b9adff var(--clip-end),
				#e5e8ef var(--clip-end),
				#e5e8ef 100%
			);
	}
	.timeline::-moz-range-progress {
		height: 6px;
		border-radius: 999px;
		background: rgba(120, 101, 221, 0.28);
	}
	.timeline::-moz-range-thumb {
		width: 10px;
		height: 10px;
		border: 3px solid #fff;
		border-radius: 50%;
		background: #6d59d1;
		box-shadow:
			0 1px 4px #1d183d44,
			0 0 0 1px #6d59d1;
	}
	:global(.content input.timeline) {
		border: 0;
		background: transparent;
		border-radius: 0;
		padding: 0;
		min-height: 0;
		box-shadow: none;
	}
	.timeline:focus-visible {
		outline: 2px solid #9b8de7;
		outline-offset: 4px;
		border-radius: 8px;
	}
	.timeline:disabled {
		opacity: 0.42;
		cursor: not-allowed;
	}
	.timeline:disabled::-webkit-slider-thumb {
		box-shadow: none;
	}
	.timeline:disabled::-moz-range-thumb {
		box-shadow: none;
	}
	.live-edge {
		display: flex;
		align-items: center;
		gap: 6px;
		flex: none;
		border: 0;
		background: transparent;
		color: #b33d49;
		font-size: 10px;
		font-weight: 650;
		cursor: pointer;
	}
	.live-edge i {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: #ea4655;
	}
	.live-edge.at-live {
		color: #c44852;
		cursor: default;
	}
	.live-edge.at-live i {
		animation: pulse 1.6s infinite;
	}
	.live-edge.waiting {
		color: #8993a2;
		cursor: default;
	}
	.live-edge.waiting i {
		background: #aab3c0;
		animation: none;
	}
	@keyframes pulse {
		50% {
			opacity: 0.35;
		}
	}
	.player-foot {
		display: flex;
		align-items: center;
		gap: 8px;
		padding: 12px 2px 0;
		color: #768397;
		font-size: 11px;
	}
	.player-foot > span:last-child {
		color: #687385;
	}
	.state-dot {
		height: 6px;
		width: 6px;
		border-radius: 50%;
		background: #47d49e;
	}
	.error {
		color: #a63242;
		background: #fff2f3;
		border: 1px solid #f4d6da;
		border-radius: 9px;
		padding: 11px 14px;
		margin: 10px 1px 0;
		font-size: 12px;
	}
	.clip-toolbar-actions {
		display: flex;
		align-items: center;
		gap: 7px;
		flex: none;
	}
	.clip-toolbar-actions .button {
		white-space: nowrap;
		padding: 8px 11px;
		font-size: 11px;
	}
	.clip-toolbar-actions .button:disabled {
		background: #eef0f4;
		color: #9aa3b2;
		border-color: #e1e4ea;
		opacity: 1;
		cursor: not-allowed;
	}
	.clip-job-state {
		flex: none;
		color: #748093;
		font-size: 10px;
	}
	.history-panel {
		width: min(100%, 80vw);
		margin-left: auto;
		margin-right: auto;
		padding: 20px;
	}
	.history-clip-panel {
		width: min(100%, 80vw);
		margin-left: auto;
		margin-right: auto;
		display: grid;
		gap: 14px;
		padding: 17px 18px;
	}
	.history-clip-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
	}
	.history-clip-head h2 {
		margin: 4px 0 0;
	}
	.history-clip-summary {
		display: grid;
		gap: 3px;
		min-width: 0;
		text-align: right;
	}
	.history-clip-summary strong {
		color: #3e4858;
		font-size: 11px;
	}
	.history-clip-summary span {
		color: #788397;
		font-size: 10px;
		font-variant-numeric: tabular-nums;
	}
	.history-clip-panel .clip-toolbar-actions {
		flex-wrap: wrap;
	}
	.history-clip-panel .error {
		margin: 0;
	}
	.history-clip-panel .clip-job-state {
		justify-self: start;
	}
	.history-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
	}
	.history-head p {
		font-size: 11px;
		margin-top: 5px;
	}
	.history-filter {
		display: flex;
		align-items: end;
		gap: 8px;
	}
	.history-filter label {
		display: grid;
		gap: 4px;
		color: #657185;
		font-size: 10px;
	}
	.history-filter input {
		width: 142px;
		min-width: 0;
		color: #273247;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 7px 8px;
		font-size: 10px;
	}
	.history-filter .day-nav {
		width: 34px;
		height: 34px;
		padding: 0;
		font-size: 18px;
		line-height: 1;
	}
	.history-visual {
		margin-top: 17px;
		padding: 12px 13px;
		background: #fafbfd;
		border: 1px solid #edf0f4;
		border-radius: 9px;
	}
	.history-ticks {
		position: relative;
		height: 12px;
		color: #8993a3;
		font-size: 9px;
	}
	.history-ticks span {
		position: absolute;
		white-space: nowrap;
		transform: translateX(-50%);
	}
	.history-ticks span:first-child {
		transform: none;
	}
	.history-ticks span:last-child {
		transform: translateX(-100%);
	}
	.history-track {
		position: relative;
		height: 15px;
		margin: 8px 0 5px;
		background: #edf0f4;
		border: 1px solid #e4e8ef;
		border-radius: 6px;
		overflow: hidden;
	}
	.history-segment {
		position: absolute;
		top: 2px;
		bottom: 2px;
		min-width: 3px;
		padding: 0;
		border: 0;
		border-radius: 4px;
		background: linear-gradient(90deg, #5dbb9c, #2fa77e);
		cursor: pointer;
	}
	.history-segment:hover,
	.history-segment.active-range {
		background: linear-gradient(90deg, #6653c7, #a694f1);
		box-shadow: 0 0 0 1px #6653c7;
	}
	.history-legend {
		display: flex;
		align-items: center;
		gap: 6px;
		color: #738095;
		font-size: 9px;
	}
	.history-legend i {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: #36b987;
	}
	.history-list {
		display: grid;
		margin-top: 15px;
		border-top: 1px solid #edf0f4;
	}
	.history-row {
		display: flex;
		align-items: center;
		gap: 12px;
		width: 100%;
		padding: 13px 8px;
		border: 0;
		border-bottom: 1px solid #edf0f4;
		background: #fff;
		text-align: left;
		color: #334155;
		cursor: pointer;
	}
	.history-row:hover,
	.history-row.active-history {
		background: #faf9ff;
	}
	.history-play {
		width: 32px;
		height: 32px;
		display: grid;
		place-items: center;
		flex: none;
		border-radius: 9px;
		background: #f0edff;
		color: #6653c7;
		font-size: 11px;
	}
	.history-copy {
		display: grid;
		gap: 3px;
		min-width: 0;
		flex: 1;
	}
	.history-copy strong {
		font-size: 11px;
	}
	.history-copy small {
		font-size: 10px;
		color: #7e899a;
	}
	.history-open {
		color: #6653c7;
		font-size: 10px;
		font-weight: 650;
		white-space: nowrap;
	}
	.history-empty {
		margin-top: 15px;
		padding: 30px 15px;
		text-align: center;
		color: #8a94a3;
		background: #fafbfd;
		border: 1px dashed #dce2ea;
		border-radius: 9px;
		font-size: 11px;
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
	.modal p {
		margin-bottom: 19px;
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
		min-width: 0;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
	}
	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: 9px;
		margin-top: 18px;
	}
	.source-choice {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 9px;
		margin: 16px 0;
	}
	.source-choice button {
		display: grid;
		gap: 6px;
		text-align: left;
		background: #fbfcfe;
		border: 1px solid #e1e6ee;
		color: #344054;
		border-radius: 10px;
		padding: 13px;
	}
	.source-choice button.chosen {
		border-color: #8672e8;
		background: #f5f2ff;
		box-shadow: 0 0 0 2px #8d7aef20;
	}
	.source-choice strong {
		font-size: 12px;
	}
	.source-choice small {
		font-size: 10px;
		color: #798496;
	}
	.secure-note {
		margin-top: 10px;
		font-size: 10px;
		line-height: 1.6;
		color: #778397;
	}
	.modal .error {
		margin: 10px 0;
	}
	.modal .button {
		margin-top: 5px;
	}
	@media (max-width: 760px) {
		.page-head,
		.video-panel,
		.stream-interrupted-notice,
		.history-clip-panel,
		.history-panel {
			width: 100%;
		}
		.stream-interrupted-notice {
			align-items: flex-start;
		}
		.stream-interrupted-notice .button {
			flex: none;
		}
		.page-head {
			align-items: flex-start;
			flex-direction: column;
			gap: 10px;
			margin-top: -8px;
		}
		.channel-heading {
			flex-wrap: wrap;
			gap: 6px 10px;
		}
		.channel-heading h1 {
			font-size: 22px;
		}
		.head-actions {
			width: 100%;
		}
		.custom-controls {
			gap: 6px;
		}
		.clip-toolbar-actions {
			width: 100%;
			flex-wrap: wrap;
		}
		.history-head {
			align-items: flex-start;
			flex-direction: column;
		}
		.history-filter {
			width: 100%;
			flex-wrap: wrap;
		}
		.history-clip-head {
			align-items: flex-start;
			flex-direction: column;
		}
		.history-clip-summary {
			text-align: left;
		}
		.history-clip-panel .clip-toolbar-actions {
			width: 100%;
		}
		.history-filter input {
			width: min(142px, 40vw);
		}
		.history-row {
			gap: 9px;
		}
		.history-open {
			font-size: 0;
		}
		.history-open::after {
			content: '→';
			font-size: 14px;
		}
		.player-foot {
			flex-wrap: wrap;
		}
	}
	.video-panel.web-fullscreen {
		position: fixed;
		inset: 0;
		z-index: 100;
		margin: 0;
		padding: 14px 18px;
		border: 0;
		border-radius: 0;
		background: #080a0f;
		box-shadow: none;
		display: flex;
		flex-direction: column;
		width: 100vw;
		height: 100vh;
		height: 100dvh;
	}
	.video-panel.web-fullscreen .video-top {
		flex: none;
	}
	.video-panel.web-fullscreen .channel-name {
		background: #171c27;
		border-color: #303848;
		color: #e2e6ee;
	}
	.video-panel.web-fullscreen .mode {
		color: #aab3c2;
	}
	.video-panel.web-fullscreen .mode.chosen {
		color: #c0b4ff;
		background: #28223f;
	}
	.video-panel.web-fullscreen .screen {
		flex: 1;
		min-height: 0;
		aspect-ratio: auto;
		border-radius: 7px;
	}
	.video-panel.web-fullscreen .custom-controls {
		flex: none;
		color: #c5ccda;
	}
	.video-panel.web-fullscreen .timeline::-webkit-slider-runnable-track {
		background:
			linear-gradient(
				to right,
				rgba(154, 136, 255, 0.3) 0%,
				rgba(154, 136, 255, 0.3) var(--progress),
				transparent var(--progress)
			),
			linear-gradient(
				to right,
				transparent 0%,
				transparent calc(var(--clip-start) - 1px),
				#c0b4ff calc(var(--clip-start) - 1px),
				#c0b4ff calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-end) - 1px),
				#c0b4ff calc(var(--clip-end) - 1px),
				#c0b4ff calc(var(--clip-end) + 1px),
				transparent calc(var(--clip-end) + 1px)
			),
			linear-gradient(
				to right,
				#343b4a 0%,
				#343b4a var(--clip-start),
				#62578d var(--clip-start),
				#62578d var(--clip-end),
				#343b4a var(--clip-end),
				#343b4a 100%
			);
	}
	.video-panel.web-fullscreen .timeline::-moz-range-track {
		background:
			linear-gradient(
				to right,
				transparent 0%,
				transparent calc(var(--clip-start) - 1px),
				#c0b4ff calc(var(--clip-start) - 1px),
				#c0b4ff calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-start) + 1px),
				transparent calc(var(--clip-end) - 1px),
				#c0b4ff calc(var(--clip-end) - 1px),
				#c0b4ff calc(var(--clip-end) + 1px),
				transparent calc(var(--clip-end) + 1px)
			),
			linear-gradient(
				to right,
				#343b4a 0%,
				#343b4a var(--clip-start),
				#62578d var(--clip-start),
				#62578d var(--clip-end),
				#343b4a var(--clip-end),
				#343b4a 100%
			);
	}
	.video-panel.web-fullscreen .timeline::-moz-range-progress {
		background: rgba(154, 136, 255, 0.3);
	}
	.video-panel.web-fullscreen .timeline::-webkit-slider-thumb {
		background: #a18eff;
		box-shadow:
			0 1px 5px #0008,
			0 0 0 1px #a18eff;
	}
	.video-panel.web-fullscreen .timeline::-moz-range-thumb {
		background: #a18eff;
		box-shadow:
			0 1px 5px #0008,
			0 0 0 1px #a18eff;
	}
	.video-panel.web-fullscreen .control-button {
		background: #171c27;
		border-color: #303848;
		color: #e3e7ef;
	}
	.video-panel.web-fullscreen .control-button:hover {
		background: #242a37;
		border-color: #7664d6;
		color: #fff;
	}
	.video-panel.web-fullscreen .player-foot,
	.video-panel.web-fullscreen .player-foot > span:last-child {
		flex: none;
		color: #aab3c2;
	}
	.icon-button svg {
		display: block;
		flex: none;
		width: 16px;
		height: 16px;
		fill: none;
		stroke: currentColor;
		stroke-width: 1.8;
		stroke-linecap: round;
		stroke-linejoin: round;
	}
	.web-fullscreen .error {
		flex: none;
	}
	@media (max-width: 520px) {
		.panel,
		.history-panel {
			padding: 13px;
		}
		.video-top {
			flex-wrap: wrap;
		}
		.screen {
			aspect-ratio: 4/3;
		}
		.control-button {
			padding: 0 7px;
		}
		.icon-button {
			width: 29px;
			padding: 0;
		}
		.time-label {
			font-size: 9px;
		}
		.live-edge {
			font-size: 9px;
		}
		.source-choice {
			grid-template-columns: 1fr;
		}
		.modal-actions {
			display: grid;
			grid-template-columns: 1fr 1fr;
		}
	}
	.copy-field {
		display: flex;
		gap: 8px;
	}
	.copy-field input {
		flex: 1;
	}
	.copy-field .button {
		flex: none;
	}
	.back-link {
		border: 0;
		background: none;
		color: #6653c7;
		font-weight: 650;
		cursor: pointer;
		padding: 0;
		margin: 0 0 10px;
		font-size: 11px;
	}
	.modal .frame-rate-setting {
		margin: 2px 0 12px;
	}
	.modal select {
		width: 100%;
		min-height: 38px;
		appearance: none;
		-webkit-appearance: none;
		background: #fbfcfd;
		background-image: linear-gradient(45deg, transparent 50%, #273247 50%),
			linear-gradient(135deg, #273247 50%, transparent 50%);
		background-repeat: no-repeat;
		background-size: 5px 5px;
		background-position: calc(100% - 22px) 50%, calc(100% - 17px) 50%;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 8px 40px 8px 11px;
		font: inherit;
	}
	.screen:fullscreen {
		width: 100vw;
		height: 100vh;
		height: 100dvh;
		aspect-ratio: auto;
		border-radius: 0;
		background: #000;
		display: grid;
		place-items: center;
	}
	.screen:fullscreen video {
		width: 100%;
		height: 100%;
		object-fit: contain;
	}
	.video-panel:fullscreen {
		position: fixed;
		inset: 0;
		z-index: 100;
		margin: 0;
		padding: max(14px, env(safe-area-inset-top)) max(18px, env(safe-area-inset-right))
			max(14px, env(safe-area-inset-bottom)) max(18px, env(safe-area-inset-left));
		border: 0;
		border-radius: 0;
		background: #080a0f;
		box-shadow: none;
		display: flex;
		flex-direction: column;
		width: 100vw;
		height: 100vh;
		height: 100dvh;
		overflow: hidden;
	}
	.video-panel:fullscreen .video-top {
		flex: none;
	}
	.video-panel:fullscreen .channel-name {
		background: #171c27;
		border-color: #303848;
		color: #e2e6ee;
	}
	.video-panel:fullscreen .mode {
		color: #aab3c2;
	}
	.video-panel:fullscreen .mode.chosen {
		color: #c0b4ff;
		background: #28223f;
	}
	.video-panel:fullscreen .screen {
		flex: 1;
		min-height: 0;
		aspect-ratio: auto;
		border-radius: 7px;
	}
	.video-panel:fullscreen .screen video {
		object-fit: contain;
	}
	.video-panel:fullscreen .custom-controls,
	.video-panel:fullscreen .player-foot {
		flex: none;
	}
	.video-panel:fullscreen .custom-controls {
		color: #c5ccda;
	}
	.video-panel:fullscreen .player-foot,
	.video-panel:fullscreen .player-foot > span:last-child {
		color: #aab3c2;
	}
	.video-panel:fullscreen .control-button {
		background: #171c27;
		border-color: #303848;
		color: #e3e7ef;
	}
	.video-panel.web-fullscreen .screen,
	.video-panel:fullscreen .screen {
		position: absolute;
		inset: 0;
		z-index: 0;
		flex: none;
		width: auto;
		height: auto;
		border-radius: 0;
	}
	.video-panel.web-fullscreen .video-top,
	.video-panel:fullscreen .video-top,
	.video-panel.web-fullscreen .custom-controls,
	.video-panel:fullscreen .custom-controls,
	.video-panel.web-fullscreen .player-foot,
	.video-panel:fullscreen .player-foot {
		position: absolute;
		z-index: 2;
		box-sizing: border-box;
		background: rgba(8, 10, 15, 0.58);
		backdrop-filter: blur(8px);
	}
	.video-panel.web-fullscreen .video-top,
	.video-panel:fullscreen .video-top {
		top: env(safe-area-inset-top);
		right: env(safe-area-inset-right);
		left: env(safe-area-inset-left);
		padding: 8px 10px;
		border-radius: 0;
	}
	.video-panel.web-fullscreen .custom-controls,
	.video-panel:fullscreen .custom-controls {
		right: env(safe-area-inset-right);
		bottom: calc(env(safe-area-inset-bottom) + 27px);
		left: env(safe-area-inset-left);
		padding: 10px 12px;
		border-radius: 0;
	}
	.video-panel.web-fullscreen .player-foot,
	.video-panel:fullscreen .player-foot {
		right: env(safe-area-inset-right);
		bottom: env(safe-area-inset-bottom);
		left: env(safe-area-inset-left);
		padding: 5px 12px;
		border-radius: 0;
	}
	@media (max-width: 760px) {
		.video-panel:fullscreen {
			padding: max(8px, env(safe-area-inset-top)) max(8px, env(safe-area-inset-right))
				max(8px, env(safe-area-inset-bottom)) max(8px, env(safe-area-inset-left));
		}
		.video-panel:fullscreen .video-top {
			gap: 5px;
			padding-bottom: 8px;
		}
		.video-panel:fullscreen .screen {
			min-height: 0;
		}
		.video-panel:fullscreen .custom-controls {
			gap: 5px;
		}
		.video-panel:fullscreen .player-foot {
			font-size: 10px;
		}
		.video-panel.web-fullscreen .video-top,
		.video-panel:fullscreen .video-top {
			top: env(safe-area-inset-top);
			right: env(safe-area-inset-right);
			left: env(safe-area-inset-left);
		}
		.video-panel.web-fullscreen .custom-controls,
		.video-panel:fullscreen .custom-controls {
			right: env(safe-area-inset-right);
			bottom: calc(env(safe-area-inset-bottom) + 25px);
			left: env(safe-area-inset-left);
		}
		.video-panel.web-fullscreen .player-foot,
		.video-panel:fullscreen .player-foot {
			right: env(safe-area-inset-right);
			bottom: env(safe-area-inset-bottom);
			left: env(safe-area-inset-left);
		}
	}
</style>
