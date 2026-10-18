<script lang="ts">
	import { userFacingError, playbackErrorMessage, clipStatusLabel } from '$lib/messages';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api';
	import { connectDvrEvents } from '$lib/events';
	import type { ClipJob, Stream, TimeRange } from '$lib/types';
	let streams = $state<Stream[]>([]);
	let active = $state<Record<string, boolean>>({});
	let selected = $state('');
	let selectedStream = $derived(streams.find((stream) => stream.id === selected));
	let role = $state<'admin' | 'viewer' | ''>('');
	let mode = $state<'live' | 'dvr'>('live');
	let video: HTMLVideoElement;
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
	let browserPushBusy = $state(false);
	let browserPushError = $state('');
	let browserPushActive = $state(false);
	let browserPushLabel = $state('');
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
	let currentTime = $state(0);
	let timelineStart = $state(0);
	let timelineEnd = $state(0);
	let liveTarget = $state(0);
	let videoPanel: HTMLElement;
	let screenPanel: HTMLElement;
	let webFullscreen = $state(false);
	let fullscreenActive = $state(false);
	let screenFullscreen = $state(false);
	let nativeVideoFullscreen = false;
	let atLiveEdge = $derived(timelineEnd > 0 && liveTarget > 0 && currentTime >= liveTarget - 3);
	let historyRanges = $state<TimeRange[]>([]);
	let historyBusy = $state(false);
	let historyDay = $state(toLocalDateInput(new Date()));
	let historyError = $state('');
	let selectedRange = $state<TimeRange | undefined>();
	let playbackOffset = 0;
	let clipStart = $state('');
	let clipEnd = $state('');
	let clipBusy = $state(false);
	let clipError = $state('');
	let favoriteDialogOpen = $state(false);
	let favoriteName = $state('');
	let clipJob = $state<ClipJob | undefined>();
	let favoriteAfterExport = false;
	let browserPeer: RTCPeerConnection | undefined;
	let browserCapture: MediaStream | undefined;
	let browserLocation = '';
	let player: { destroy: () => void; liveSyncPosition?: number | null } | undefined;
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
						] as const;
					} catch {
						return [stream.id, false] as const;
					}
				}),
			);
			active = Object.fromEntries(states);
			status = '服务连接正常';
			error = '';
			if (
				selected &&
				(selected !== previousSelected ||
					!player ||
					(mode === 'live' && previousLive !== active[selected]))
			)
				void play();
			if (selected) void loadHistory();
		} catch (e) {
			error = userFacingError(e);
			status = '连接失败';
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
		browserPushError = '';
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
	function waitForICE(pc: RTCPeerConnection) {
		if (pc.iceGatheringState === 'complete') return Promise.resolve();
		return new Promise<void>((resolve, reject) => {
			const timeout = window.setTimeout(() => reject(new Error('ICE 候选收集超时')), 20000);
			pc.addEventListener(
				'icegatheringstatechange',
				() => {
					if (pc.iceGatheringState === 'complete') {
						window.clearTimeout(timeout);
						resolve();
					}
				},
				{ once: false },
			);
			pc.addEventListener('connectionstatechange', () => {
				if (pc.connectionState === 'failed') {
					window.clearTimeout(timeout);
					reject(new Error('WebRTC 连接失败'));
				}
			});
		});
	}
	async function startBrowserPublish() {
		if (!selected || !browserPushKey.trim()) return;
		browserPushBusy = true;
		browserPushError = '';
		try {
			if (!window.isSecureContext) throw new Error('浏览器推流需要 HTTPS 或 localhost');
			const videoConstraints: MediaTrackConstraints = {
				frameRate: { ideal: browserPushFrameRate, max: browserPushFrameRate },
			};
			if (browserPushResolution !== 'original') {
				const maxDimension =
					browserPushResolution === '1080p'
						? { width: 1920, height: 1080 }
						: { width: 1280, height: 720 };
				videoConstraints.width = { ideal: maxDimension.width, max: maxDimension.width };
				videoConstraints.height = { ideal: maxDimension.height, max: maxDimension.height };
			}
			browserCapture =
				browserPushMode === 'screen'
					? await navigator.mediaDevices.getDisplayMedia({ video: videoConstraints, audio: true })
					: await navigator.mediaDevices.getUserMedia({ video: videoConstraints, audio: true });
			if (!browserCapture.getAudioTracks().length) {
				try {
					const mic = await navigator.mediaDevices.getUserMedia({ audio: true });
					for (const track of mic.getAudioTracks()) browserCapture.addTrack(track);
				} catch {}
			}
			browserPeer = new RTCPeerConnection({ iceCandidatePoolSize: 1 });
			for (const track of browserCapture.getTracks()) browserPeer.addTrack(track, browserCapture);
			const offer = await browserPeer.createOffer();
			await browserPeer.setLocalDescription(offer);
			await waitForICE(browserPeer);
			const response = await fetch(`/api/whip/${encodeURIComponent(selected)}`, {
				method: 'POST',
				headers: {
					'Content-Type': 'application/sdp',
					Authorization: `Bearer ${browserPushKey.trim()}`,
				},
				body: browserPeer.localDescription?.sdp,
				credentials: 'same-origin',
			});
			if (!response.ok) {
				const body = await response.json().catch(() => ({}));
				throw new Error(body.error ?? `推流协商失败 (${response.status})`);
			}
			browserLocation = response.headers.get('Location') ?? '';
			await browserPeer.setRemoteDescription({ type: 'answer', sdp: await response.text() });
			browserPushActive = true;
			browserPushKey = '';
			browserPushLabel = browserPushMode === 'screen' ? '屏幕' : '摄像头';
			browserPushOpen = false;
			browserPeer.addEventListener('connectionstatechange', () => {
				if (browserPeer?.connectionState === 'failed')
					browserPushError = 'WebRTC 连接中断，请停止后重新推流';
			});
			void refresh();
		} catch (e) {
			browserPushError = userFacingError(e);
			if (browserLocation) {
				try {
					await fetch(browserLocation, { method: 'DELETE', credentials: 'same-origin' });
				} catch {}
				browserLocation = '';
			}
			browserPeer?.close();
			browserPeer = undefined;
			browserCapture?.getTracks().forEach((track) => track.stop());
			browserCapture = undefined;
		} finally {
			browserPushBusy = false;
		}
	}
	async function stopBrowserPublish() {
		const peer = browserPeer;
		browserPeer = undefined;
		peer?.close();
		browserCapture?.getTracks().forEach((track) => track.stop());
		browserCapture = undefined;
		if (browserLocation) {
			try {
				await fetch(browserLocation, { method: 'DELETE', credentials: 'same-origin' });
			} catch {}
		}
		browserLocation = '';
		browserPushActive = false;
		browserPushError = '';
		void refresh();
	}
	async function play() {
		if (!selected || !video) return;
		video.pause();
		player?.destroy();
		player = undefined;
		video.removeAttribute('src');
		video.load();
		mutedPlayback = true;
		video.muted = true;
		isPlaying = false;
		currentTime = 0;
		timelineStart = 0;
		timelineEnd = 0;
		liveTarget = 0;
		error = '';
		if (mode === 'live' && !active[selected]) return;
		let url = `/api/playback/${mode}/${selected}.m3u8`;
		try {
			if (selectedRange) {
				const resolved = await api<{ offset: number }>(
					`/api/playback/resolve?${new URLSearchParams({ streamId: selected, at: selectedRange.start })}`,
				);
				playbackOffset = resolved.offset;
				url += `?${new URLSearchParams({ start: selectedRange.start, end: selectedRange.end })}`;
			}
			const { default: Hls } = await import('hls.js');
			if (Hls.isSupported()) {
				const mediaElement = video;
				const hls = new Hls({
					liveSyncDurationCount: 3,
					xhrSetup: (xhr) => {
						xhr.withCredentials = true;
					},
				});
				hls.on(Hls.Events.MANIFEST_PARSED, () => {
					if (selectedRange) mediaElement.currentTime = playbackOffset;
					void mediaElement.play().catch(() => {
						error = autoplayBlockedMessage;
					});
				});
				hls.on(Hls.Events.ERROR, (_event, data) => {
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
				player = hls;
			} else if (video.canPlayType('application/vnd.apple.mpegurl')) {
				video.src = url;
				video.onloadedmetadata = () => {
					if (selectedRange) video.currentTime = playbackOffset;
					void video.play().catch(() => {
						error = autoplayBlockedMessage;
					});
				};
			} else error = '当前浏览器暂不支持播放此直播。';
		} catch (e) {
			error = userFacingError(e);
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
					? Math.max(timelineStart, timelineEnd - 10)
					: 0;
	}
	function seekTo(value: number) {
		if (!video || timelineEnd <= timelineStart) return;
		video.currentTime = Math.max(timelineStart, Math.min(timelineEnd, value));
		updateTimeline();
	}
	function handleVideoPlay() {
		isPlaying = true;
		if (error === autoplayBlockedMessage || error === playbackFailedMessage) error = '';
	}
	function togglePlayback() {
		if (!video) return;
		if (video.paused) {
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
	function toggleWebFullscreen() {
		webFullscreen = !webFullscreen;
	}
	async function toggleFullscreen() {
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
			else {
				webFullscreen = true;
			}
		} catch {
			if (nativeVideo.webkitEnterFullscreen) nativeVideo.webkitEnterFullscreen();
			else error = '无法进入全屏模式，请使用播放器控件重试。';
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
		if (event.key === 'Escape' && webFullscreen) {
			event.preventDefault();
			webFullscreen = false;
			return;
		}
		if (!selected) return;
		const key = event.key.toLowerCase();
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
		if (key === 'w') {
			event.preventDefault();
			toggleWebFullscreen();
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
		} catch (e) {
			historyError = userFacingError(e);
		} finally {
			historyBusy = false;
		}
	}
	async function playRange(range: TimeRange) {
		selectedRange = range;
		mode = 'dvr';
		clipStart = toPreciseLocalInput(range.start);
		clipEnd = toPreciseLocalInput(range.end);
		clipError = '';
		clipJob = undefined;
		await play();
	}
	function historyPosition(range: TimeRange) {
		const { start, end } = historyDayBounds();
		const startMs = start.getTime();
		const endMs = end.getTime();
		const rangeStart = Math.max(startMs, new Date(range.start).getTime());
		return Math.max(
			0,
			Math.min(100, ((rangeStart - startMs) / Math.max(1, endMs - startMs)) * 100),
		);
	}
	function historyWidth(range: TimeRange) {
		const { start, end } = historyDayBounds();
		const startMs = start.getTime();
		const endMs = end.getTime();
		const rangeStart = Math.max(startMs, new Date(range.start).getTime());
		const rangeEnd = Math.min(endMs, new Date(range.end).getTime());
		return Math.max(
			0.35,
			Math.min(
				100 - historyPosition(range),
				((rangeEnd - rangeStart) / Math.max(1, endMs - startMs)) * 100,
			),
		);
	}
	function playbackWallTime() {
		if (!selectedRange || !video) return '';
		return new Date(
			new Date(selectedRange.start).getTime() + (video.currentTime - playbackOffset) * 1000,
		).toISOString();
	}
	function clipPercent(value: string) {
		if (!selectedRange || timelineEnd <= timelineStart) return 0;
		const start = new Date(selectedRange.start).getTime();
		const at = new Date(value).getTime();
		if (!Number.isFinite(start) || !Number.isFinite(at)) return 0;
		const mediaPosition = playbackOffset + (at - start) / 1000;
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
	function openFavoriteDialog() {
		if (!clipRangeReady()) return;
		favoriteName = '';
		favoriteDialogOpen = true;
	}
	async function saveFavorite() {
		favoriteDialogOpen = false;
		await createHistoryClip(true, favoriteName);
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
					mode: 'accurate',
					gapPolicy: 'reject',
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
		if (!clipJob) return;
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
		window.addEventListener('keydown', handleKeyboardShortcuts);
		document.addEventListener('fullscreenchange', handleFullscreenChange);
		video?.addEventListener('webkitbeginfullscreen', onNativeBegin);
		video?.addEventListener('webkitendfullscreen', onNativeEnd);
		return () => {
			disconnect();
			window.removeEventListener('keydown', handleKeyboardShortcuts);
			document.removeEventListener('fullscreenchange', handleFullscreenChange);
			video?.removeEventListener('webkitbeginfullscreen', onNativeBegin);
			video?.removeEventListener('webkitendfullscreen', onNativeEnd);
			player?.destroy();
			browserPeer?.close();
			browserCapture?.getTracks().forEach((track) => track.stop());
		};
	});
</script>

<section class="page-head">
	<div class="channel-heading">
		<h1>{selectedStream?.name ?? '频道详情'}</h1>
		<span>直播 · 时移 · 直播历史</span>
	</div>
	<div class="head-actions">
		{#if role === 'admin' && !browserPushActive}<button
				class="button"
				disabled={!selectedStream}
				title={selectedStream ? '选择浏览器或 OBS 推流' : '请先创建并选择频道'}
				onclick={openPushDialog}
			>
				推流
			</button>{/if}{#if browserPushActive}<button
				class="button danger"
				onclick={stopBrowserPublish}
			>
				停止{browserPushLabel}推流
			</button>{/if}
		<button class="button secondary" onclick={refresh}>刷新状态</button>
	</div>
</section>
<div class="player-layout">
	<section class="panel video-panel" class:web-fullscreen={webFullscreen} bind:this={videoPanel}>
		<div class="video-top">
			<div class="live-pill">
				<i></i>
				{mode === 'live' ? '直播中' : '时移回看'}
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
			<button
				class:chosen={mode === 'dvr'}
				class="mode"
				onclick={() => {
					mode = 'dvr';
					void play();
				}}
			>
				时移
			</button>
		</div>
		<div class="screen" bind:this={screenPanel}>
			<video
				bind:this={video}
				muted={mutedPlayback}
				playsinline
				ontimeupdate={updateTimeline}
				onprogress={updateTimeline}
				onloadedmetadata={updateTimeline}
				onseeking={updateTimeline}
				onplay={handleVideoPlay}
				onpause={() => (isPlaying = false)}
			></video>
			{#if !selected || (mode === 'live' && !active[selected])}<div class="empty-screen">
					<div class="play-icon">▶</div>
					<span>{selected ? '频道尚未开播' : '选择频道并开始播放'}</span>
					{#if selected}<small>开始推流后会自动播放</small>{/if}
				</div>{/if}{#if screenFullscreen}<button
					class="fullscreen-exit"
					aria-label="退出视频全屏"
					title="退出全屏"
					onclick={() => void toggleFullscreen()}
				>
					<svg viewBox="0 0 24 24" aria-hidden="true">
						<path d="M8 4v4H4m12-4v4h4M8 20v-4H4m12 4v-4h4" />
						<path d="m4 8 4-4m8 0 4 4M4 16l4 4m8 0 4-4" />
					</svg>
				</button>{/if}
		</div>
		{#if selected}<div
				class="custom-controls"
				aria-label="播放器控制：空格或 K 播放暂停，方向键跳转，M 声音，W 页面放大，F 浏览器全屏"
			>
				<button
					class="control-button icon-button"
					aria-label={mode === 'live' && !active[selected]
						? '等待推流'
						: isPlaying
							? '暂停'
							: '播放'}
					title={mode === 'live' && !active[selected]
						? '等待推流'
						: `${isPlaying ? '暂停' : '播放'}（空格 / K）`}
					disabled={mode === 'live' && !active[selected]}
					onclick={togglePlayback}
				>
					{mode === 'live' && !active[selected] ? '▶' : isPlaying ? 'Ⅱ' : '▶'}
				</button>
				{#if mode === 'dvr'}<span class="time-label">
						{formatTime(currentTime - timelineStart)}
					</span>{/if}
				<input
					class="timeline"
					aria-label="播放进度"
					title="←/→ 快退或快进 5 秒，Shift 加 10 秒"
					type="range"
					disabled={mode === 'live' && !active[selected]}
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
				{#if mode === 'dvr'}<span class="time-label">
						{formatTime(timelineEnd - timelineStart)}
					</span>{:else if active[selected]}<button
						class:at-live={atLiveEdge}
						class="live-edge"
						aria-label={atLiveEdge ? '正在直播' : '回到直播'}
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
					aria-label={webFullscreen ? '退出网页内全屏' : '网页内全屏'}
					title={webFullscreen ? '退出网页内全屏（W）' : '网页内全屏（W）'}
					onclick={toggleWebFullscreen}
				>
					<svg viewBox="0 0 24 24" aria-hidden="true">
						{#if webFullscreen}<path
								d="M9 3H3v6m0-6 7 7m5-7h6v6m0-6-7 7M9 21H3v-6m0 6 7-7m5 7h6v-6m0 6-7-7"
							/>{:else}<path
								d="M8 3H3v5m0-5 6 6m7-6h5v5m0-5-6 6M8 21H3v-5m0 5 6-6m7 6h5v-5m0 5-6-6"
							/>{/if}
					</svg>
				</button>
				<button
					class="control-button icon-button"
					aria-label={fullscreenActive ? '退出浏览器全屏' : '浏览器全屏'}
					title={fullscreenActive ? '退出浏览器全屏（F）' : '浏览器全屏（F）'}
					onclick={toggleFullscreen}
				>
					<svg viewBox="0 0 24 24" aria-hidden="true">
						{#if fullscreenActive}<path d="M8 4v4H4m12-4v4h4M8 20v-4H4m12 4v-4h4" />
							<path d="m4 8 4-4m8 0 4 4M4 16l4 4m8 0 4-4" />{:else}<path
								d="M3 8V3h5m8 0h5v5M3 16v5h5m8 0h5v-5"
							/>
							<path d="m3 8 5-5m8 0 5 5M3 16l5 5m8 0 5-5" />{/if}
					</svg>
				</button>
			</div>{/if}
		<div class="player-foot">
			<span class="state-dot"></span>
			{status}
			<span class="spacer"></span>
			<span>HLS 播放 · H.264 / HEVC / AV1 · AAC / Opus</span>
		</div>
		{#if error || browserPushError}<p class="error">
				{error || browserPushError}
			</p>{/if}{#if mode === 'dvr'}<div class="clip-toolbar" aria-label="直播片段剪辑">
				<div class="clip-range-info">
					{#if selectedRange}<strong>已选区间</strong>
						<span>
							{new Date(clipStart).toLocaleString()} — {new Date(clipEnd).toLocaleTimeString()}
						</span>{:else}<strong>未选择区间</strong>
						<span>从下方直播历史中选择一段录像</span>{/if}
				</div>
				<div class="clip-toolbar-actions">
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
					{#if clipJob?.status === 'COMPLETED' && !clipJob.request.saveToLibrary}<a
							class="button"
							href={`/api/clips/${clipJob.id}/download`}
							download={`clip-${clipJob.id}.mp4`}
						>
							下载片段
						</a>{:else}<button
							class="button"
							disabled={!clipRangeReady() ||
								clipBusy ||
								clipJob?.status === 'QUEUED' ||
								clipJob?.status === 'RUNNING'}
							onclick={() => createHistoryClip(false)}
						>
							{clipJob?.status === 'RUNNING' || clipJob?.status === 'QUEUED'
								? `导出中 ${Math.round(clipJob.progress * 100)}%`
								: '下载'}
						</button>{/if}
					<button
						class="button secondary"
						disabled={!clipRangeReady() ||
							clipBusy ||
							clipJob?.status === 'QUEUED' ||
							clipJob?.status === 'RUNNING' ||
							Boolean(clipJob?.status === 'COMPLETED' && clipJob.pinned)}
						onclick={openFavoriteDialog}
					>
						{clipJob?.status === 'COMPLETED' && clipJob.pinned
							? '已收藏'
							: clipJob?.request.saveToLibrary && clipJob.status === 'COMPLETED'
								? '收藏中…'
								: clipJob?.request.saveToLibrary && clipJob.status === 'RUNNING'
									? '收藏中…'
									: '收藏'}
					</button>
				</div>
				{#if clipError}<p class="error">{clipError}</p>{/if}{#if clipJob}<span
						class="clip-job-state"
					>
						{clipStatusLabel(clipJob.status)} · {Math.round(clipJob.progress * 100)}%
					</span>{/if}
			</div>{/if}
	</section>
</div>
<section class="panel history-panel">
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
				<span>00:00</span>
				<span>06:00</span>
				<span>12:00</span>
				<span>18:00</span>
				<span>24:00</span>
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
			{#each historyRanges as range}<button
					class:active-history={selectedRange?.start === range.start}
					class="history-row"
					onclick={() => void playRange(range)}
				>
					<span class="history-play">▶</span>
					<span class="history-copy">
						<strong>
							{new Date(range.start).toLocaleString()} — {new Date(range.end).toLocaleTimeString()}
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
</section>

{#if favoriteDialogOpen}<div class="modal-backdrop">
		<div
			class="modal"
			role="dialog"
			aria-modal="true"
			aria-labelledby="favorite-modal-title"
			tabindex="-1"
		>
			<div class="eyebrow">保存片段</div>
			<h2 id="favorite-modal-title">收藏片段</h2>
			<p>为片段起个容易识别的名字，方便之后查找。留空时会自动生成名称。</p>
			<label>
				片段名称
				<input bind:value={favoriteName} maxlength="100" placeholder={generatedClipName()} />
			</label>
			<div class="modal-actions">
				<button class="button secondary" onclick={() => (favoriteDialogOpen = false)}>取消</button>
				<button class="button" disabled={clipBusy} onclick={saveFavorite}>
					{clipBusy ? '正在保存…' : '确认收藏'}
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
			{#if browserPushError}<p class="error">{browserPushError}</p>{/if}
			<p class="secure-note">
				分辨率上限会在采集时缩放画面，不改变共享来源。实际帧率取决于设备和浏览器。
			</p>
			<p class="secure-note">需要 HTTPS（localhost 除外）；采集权限只在推流期间使用。</p>
			<div class="modal-actions">
				<button
					class="button secondary"
					disabled={browserPushBusy}
					onclick={() => (browserPushOpen = false)}
				>
					取消
				</button>
				<button
					class="button"
					disabled={browserPushBusy || !browserPushKey || !selected}
					onclick={startBrowserPublish}
				>
					{browserPushBusy ? '正在连接…' : '开始推流'}
				</button>
			</div>
		</section>
	</div>{/if}

<style>
	.page-head {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 16px;
		margin: -14px 0 14px;
		min-height: 42px;
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
		padding: 14px;
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
		width: 32px;
		padding: 0;
		font-size: 13px;
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
	.clip-toolbar {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 14px;
		margin-top: 12px;
		padding: 11px 12px;
		background: #fafbfd;
		border: 1px solid #e7eaf0;
		border-radius: 10px;
	}
	.clip-range-info {
		display: grid;
		gap: 3px;
		min-width: 0;
	}
	.clip-range-info strong {
		color: #3e4858;
		font-size: 11px;
	}
	.clip-range-info span {
		color: #788397;
		font-size: 10px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-variant-numeric: tabular-nums;
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
	.clip-toolbar .error {
		flex-basis: 100%;
		margin: 0;
	}
	.history-panel {
		padding: 20px;
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
		display: flex;
		justify-content: space-between;
		gap: 10px;
		color: #8993a3;
		font-size: 9px;
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
		.clip-toolbar {
			align-items: flex-start;
			flex-direction: column;
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
	.video-panel.web-fullscreen .clip-toolbar {
		flex: none;
		background: #11151e;
		border-color: #303848;
	}
	.video-panel.web-fullscreen .clip-range-info strong {
		color: #c5baff;
	}
	.video-panel.web-fullscreen .clip-range-info span,
	.video-panel.web-fullscreen .clip-job-state {
		color: #aab3c2;
	}
	.video-panel.web-fullscreen .clip-toolbar .button.secondary {
		background: #171c27;
		border-color: #303848;
		color: #e3e7ef;
	}
	.video-panel.web-fullscreen .clip-toolbar .button:disabled {
		background: #292e39;
		color: #838c9a;
		border-color: #303848;
	}
	.icon-button svg {
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
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 8px 11px;
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
	.fullscreen-exit {
		position: absolute;
		z-index: 2;
		top: max(14px, env(safe-area-inset-top));
		right: max(14px, env(safe-area-inset-right));
		width: 42px;
		height: 42px;
		display: grid;
		place-items: center;
		border: 1px solid #ffffff44;
		border-radius: 50%;
		background: #111a;
		color: #fff;
		cursor: pointer;
	}
	.fullscreen-exit svg {
		width: 20px;
		height: 20px;
		fill: none;
		stroke: currentColor;
		stroke-width: 1.8;
		stroke-linecap: round;
		stroke-linejoin: round;
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
	.video-panel:fullscreen .clip-toolbar {
		flex: none;
		background: #11151e;
		border-color: #303848;
	}
	.video-panel:fullscreen .clip-range-info strong {
		color: #c5baff;
	}
	.video-panel:fullscreen .clip-range-info span,
	.video-panel:fullscreen .clip-job-state {
		color: #aab3c2;
	}
	.video-panel:fullscreen .clip-toolbar .button.secondary {
		background: #171c27;
		border-color: #303848;
		color: #e3e7ef;
	}
	.video-panel:fullscreen .clip-toolbar .button:disabled {
		background: #292e39;
		color: #838c9a;
		border-color: #303848;
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
	}
</style>
