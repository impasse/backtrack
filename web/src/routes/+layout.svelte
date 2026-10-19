<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { logoutSession, restoreAuthSession, type AuthRole, type AuthSession } from '#lib/auth';
	import { connectDvrEvents, notifyAuthChange, streamsChangeEvent } from '#lib/events';
	import { api } from '#lib/api';
	import {
		browserPublish,
		stopBrowserPublish,
		notifyBrowserPublishEnded,
	} from '#lib/browser-publish.svelte';
	import type { Stream } from '#lib/types';
	let { children } = $props();
	let authReady = $state(false);
	let authenticated = $state(false);
	let savedRole = $state<AuthRole | ''>('');
	let streams = $state<Stream[]>([]);
	let userMenuOpen = $state(false);
	let userMenu = $state<HTMLDivElement>();
	let selectedStreamId = $derived(page.url.searchParams.get('streamId') ?? '');
	let streamTabs = [
		{ href: '/live', label: '实时直播' },
		{ href: '/recordings', label: '直播历史' },
		{ href: '/favorites', label: '收藏片段' },
	];
	let onStreamPage = $derived(streamTabs.some((tab) => page.url.pathname === tab.href));
	let activeTab = $derived(
		page.url.pathname === '/recordings' || page.url.searchParams.get('tab') === 'history'
			? '/recordings'
			: page.url.pathname === '/favorites'
				? '/favorites'
				: '/live',
	);
	let publishingStreamName = $derived(
		streams.find((stream) => stream.id === browserPublish.streamId)?.name ?? '当前频道',
	);
	function confirmBrowserPublishExit(event: BeforeUnloadEvent) {
		if (!browserPublish.active && !browserPublish.busy) return;
		event.preventDefault();
		event.returnValue = '';
	}
	function finishBrowserPublishExit() {
		if (browserPublish.active || browserPublish.busy) notifyBrowserPublishEnded();
	}
	async function stopPublishingFromShell() {
		await stopBrowserPublish();
		browserPublish.error = '';
	}
	async function loadStreams() {
		if (!authenticated) return;
		try {
			streams = (await api<{ streams: Stream[] }>('/api/streams')).streams ?? [];
		} catch {
			streams = [];
		}
	}
	function streamHref(path: string, streamId = selectedStreamId) {
		return streamId ? `${path}?streamId=${encodeURIComponent(streamId)}` : '/channels';
	}
	function tabHref(path: string) {
		if (path === '/recordings') {
			return selectedStreamId ? `${streamHref('/live')}&tab=history` : '/channels';
		}
		return streamHref(path);
	}
	async function openPushDialog() {
		if (!selectedStreamId) return;
		if (
			page.url.pathname !== '/live' ||
			page.url.searchParams.get('streamId') !== selectedStreamId ||
			page.url.searchParams.get('tab') === 'history'
		) {
			await goto(streamHref('/live'));
		}
		window.dispatchEvent(new Event('dvr-open-push-dialog'));
	}
	async function changeStream(event: Event) {
		const streamId = (event.currentTarget as HTMLSelectElement).value;
		if (!streamId) return;
		const currentTab = activeTab;
		const destination =
			currentTab === '/recordings'
				? `${streamHref('/live', streamId)}&tab=history`
				: streamHref(currentTab === '/favorites' ? '/favorites' : '/live', streamId);
		await goto(destination);
	}
	function nextPath() {
		const next = page.url.searchParams.get('next');
		return next?.startsWith('/') && !next.startsWith('//') && !next.startsWith('/login')
			? next
			: '/channels';
	}
	onMount(() => {
		let active = true;
		const onAuthChange = (event: Event) => {
			const state = (event as CustomEvent<AuthSession>).detail;
			authenticated = state.authenticated;
			savedRole = state.role ?? '';
			if (state.authenticated) void loadStreams();
			else streams = [];
		};
		const onStreamsChange = () => void loadStreams();
		const disconnectDvrEvents = connectDvrEvents({
			'stream.created': () => void loadStreams(),
			'stream.updated': () => void loadStreams(),
			'stream.deleted': () => void loadStreams(),
		});
		window.addEventListener('dvr-auth-changed', onAuthChange);
		window.addEventListener(streamsChangeEvent, onStreamsChange);
		void restoreAuthSession().then((session) => {
			if (!active) return;
			authenticated = session.authenticated;
			savedRole = session.role ?? '';
			authReady = true;
			if (session.authenticated) void loadStreams();
			notifyAuthChange(session);
		});
		return () => {
			active = false;
			disconnectDvrEvents();
			window.removeEventListener('dvr-auth-changed', onAuthChange);
			window.removeEventListener(streamsChangeEvent, onStreamsChange);
		};
	});
	$effect(() => {
		if (!authReady) return;
		if (!authenticated && page.url.pathname !== '/login') {
			void goto(`/login?next=${encodeURIComponent(page.url.pathname + page.url.search)}`);
		} else if (authenticated && page.url.pathname === '/login') {
			void goto(nextPath());
		}
	});
	async function clearKey() {
		userMenuOpen = false;
		if (browserPublish.active || browserPublish.busy) await stopPublishingFromShell();
		await logoutSession();
		authenticated = false;
		savedRole = '';
		notifyAuthChange({ authenticated: false, role: '' });
		await goto('/login');
	}
	function closeUserMenuOutside(event: PointerEvent) {
		if (userMenu && !userMenu.contains(event.target as Node)) userMenuOpen = false;
	}
	function closeUserMenuOnEscape(event: KeyboardEvent) {
		if (event.key === 'Escape' && userMenuOpen) {
			userMenuOpen = false;
			userMenu?.querySelector<HTMLButtonElement>('.user-menu-trigger')?.focus();
		}
	}
</script>

<svelte:window
	onpointerdown={closeUserMenuOutside}
	onkeydown={closeUserMenuOnEscape}
	onbeforeunload={confirmBrowserPublishExit}
	onpagehide={finishBrowserPublishExit}
/>
<svelte:head>
	<title>{page.url.pathname === '/login' ? '登录 · Backtrack DVR' : 'Backtrack DVR'}</title>
	<meta name="description" content="Backtrack 私人直播录像与时移回看服务" />
	<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
</svelte:head>
{#if !authReady}
	<div class="auth-loading">
		<img class="loading-mark" src="/icon.svg" alt="" />
		<span>正在检查登录状态…</span>
	</div>
{:else if page.url.pathname === '/login'}
	{@render children()}
{:else if authenticated}
	<div class="app-shell">
		<main>
			<header class="topbar">
				<a class="brand" href="/channels">
					<img class="brand-mark" src="/icon.svg" alt="" />
					<strong>
						Backtrack <span>DVR</span>
					</strong>
				</a>
				<div class="workspace-actions">
					<label class="channel-picker">
						<span class="sr-only">当前频道</span>
						<select
							aria-label="选择频道"
							value={selectedStreamId}
							disabled={browserPublish.active || browserPublish.busy}
							onchange={changeStream}
						>
							<option value="">{streams.length ? '选择频道' : '还没有频道'}</option>
							{#each streams as stream}<option value={stream.id}>{stream.name}</option>{/each}
						</select>
					</label>
					{#if onStreamPage && selectedStreamId && savedRole !== 'viewer'}
						<button class="button push-button" type="button" onclick={openPushDialog}>推流</button>
					{/if}
					<div class="user-menu" bind:this={userMenu}>
						<button
							class="user-menu-trigger"
							type="button"
							aria-label="用户菜单"
							aria-expanded={userMenuOpen}
							title="用户菜单"
							onclick={() => (userMenuOpen = !userMenuOpen)}
						>
							<svg viewBox="0 0 24 24" aria-hidden="true">
								<circle cx="12" cy="8" r="3.5" />
								<path d="M5.5 20c.6-3.1 3-5 6.5-5s5.9 1.9 6.5 5" />
							</svg>
						</button>
						{#if userMenuOpen}
							<div class="user-menu-panel">
								<div class="user-role">{savedRole === 'viewer' ? '只读访问' : '管理员'}</div>
								<a
									class="user-menu-item"
									href="/settings?section=channels"
									onclick={() => (userMenuOpen = false)}
								>
									设置
								</a>
								<button class="sign-out" type="button" onclick={clearKey}>退出登录</button>
							</div>
						{/if}
					</div>
				</div>
			</header>
			{#if onStreamPage}
				<nav class="workspace-tabs" aria-label="频道页面">
					{#each streamTabs as tab}
						<a
							class:active={activeTab === tab.href}
							aria-current={activeTab === tab.href ? 'page' : undefined}
							href={tabHref(tab.href)}
						>
							{tab.label}
						</a>
					{/each}
				</nav>
			{/if}
			{#if browserPublish.active || browserPublish.busy}
				<aside class="publish-status" role="status" aria-live="polite">
					<span class="publish-status-copy">
						<i class:connecting={browserPublish.busy}></i>
						<strong>
							{browserPublish.busy
								? '正在连接推流'
								: browserPublish.error
									? '推流异常'
									: '正在推流'}
						</strong>
						<span>
							{publishingStreamName}{browserPublish.label ? ` · ${browserPublish.label}` : ''}
						</span>
						<small>关闭此页或浏览器会停止推流</small>
						{#if browserPublish.error}<small class="publish-error">
								{browserPublish.error}
							</small>{/if}
					</span>
					<button
						class="button danger"
						type="button"
						onclick={() => void stopPublishingFromShell()}
					>
						停止推流
					</button>
				</aside>
			{/if}
			<div class="content">{@render children()}</div>
		</main>
	</div>
{/if}

<style>
	:global(*) {
		box-sizing: border-box;
	}
	:global(:root) {
		font-family:
			Inter,
			-apple-system,
			BlinkMacSystemFont,
			'Segoe UI',
			sans-serif;
		color: #202938;
		background: #f3f5f8;
		font-synthesis: none;
		text-rendering: optimizeLegibility;
		--ink: #202938;
		--muted: #697586;
		--line: #e3e8ef;
		--surface: #fff;
		--canvas: #f3f5f8;
		--accent: #6653c7;
		--accent-soft: #f0edff;
		--green: #16835f;
	}
	:global(body) {
		margin: 0;
		background: var(--canvas);
		color: var(--ink);
		font:
			14px/1.55 Inter,
			-apple-system,
			BlinkMacSystemFont,
			'Segoe UI',
			sans-serif;
	}
	:global(button),
	:global(input),
	:global(select) {
		font: inherit;
	}
	:global(button) {
		cursor: pointer;
	}
	:global(button:focus-visible),
	:global(a:focus-visible),
	:global(input:focus-visible),
	:global(select:focus-visible) {
		outline: 3px solid #a99afa;
		outline-offset: 2px;
	}
	.app-shell {
		min-height: 100vh;
	}
	.brand {
		display: flex;
		align-items: center;
		gap: 12px;
		color: #fff;
		text-decoration: none;
		font-weight: 720;
		font-size: 17px;
		margin: 0;
		letter-spacing: -0.25px;
	}
	.brand strong {
		color: #202938;
		letter-spacing: -0.45px;
	}
	.brand strong span {
		color: #7c8798;
		font-size: 11px;
		letter-spacing: 1px;
		margin-left: 2px;
	}
	.brand-mark {
		width: 37px;
		height: 37px;
		object-fit: contain;
	}
	main {
		min-width: 0;
	}
	.topbar {
		min-height: 72px;
		border-bottom: 1px solid var(--line);
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 20px;
		padding: 0 clamp(20px, 4vw, 64px);
		background: #fff;
	}
	.brand-mark {
		width: 36px;
		height: 36px;
		object-fit: contain;
	}
	.workspace-actions {
		display: flex;
		align-items: center;
		gap: 10px;
	}
	.workspace-actions .button {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		text-decoration: none;
	}
	.user-menu {
		position: relative;
		margin-left: 8px;
	}
	.user-menu-trigger {
		width: 38px;
		height: 38px;
		display: grid;
		place-items: center;
		border: 1px solid #dce2ea;
		border-radius: 50%;
		background: #fff;
		color: #566276;
		transition:
			background 0.15s,
			border-color 0.15s;
	}
	.user-menu-trigger:hover,
	.user-menu-trigger[aria-expanded='true'] {
		background: #f3f0ff;
		border-color: #c9c0f4;
		color: #6653c7;
	}
	.user-menu-trigger svg {
		width: 20px;
		height: 20px;
		fill: none;
		stroke: currentColor;
		stroke-width: 1.7;
		stroke-linecap: round;
		stroke-linejoin: round;
	}
	.user-menu-panel {
		position: absolute;
		z-index: 30;
		top: calc(100% + 9px);
		right: 0;
		width: 180px;
		padding: 7px;
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 11px;
		box-shadow: 0 12px 32px #1f29371c;
	}
	.user-role {
		padding: 9px 10px;
		color: #697586;
		font-size: 12px;
		font-weight: 600;
	}
	.user-menu-item {
		display: block;
		width: 100%;
		border-radius: 7px;
		padding: 9px 10px;
		color: #344054;
		font-size: 12px;
		text-decoration: none;
	}
	.user-menu-item:hover {
		background: #f3f0ff;
		color: #5c49bc;
	}
	.sign-out {
		width: 100%;
		border: 0;
		border-radius: 7px;
		background: transparent;
		color: #a33b4a;
		text-align: left;
		margin-top: 4px;
		padding: 9px 10px;
		border-top: 1px solid #edf0f4;
		font: inherit;
		font-size: 12px;
		cursor: pointer;
	}
	.sign-out:hover {
		background: #fff1f2;
	}
	.channel-picker {
		position: relative;
		display: inline-flex;
	}
	.channel-picker select {
		min-width: 150px;
		max-width: 240px;
		min-height: 38px;
		padding: 7px 40px 7px 11px;
		appearance: none;
		-webkit-appearance: none;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		background: #fff;
		color: #293346;
		font: inherit;
	}
	.channel-picker::after {
		content: '';
		position: absolute;
		top: 50%;
		right: 16px;
		width: 8px;
		height: 8px;
		border-right: 1.7px solid #344054;
		border-bottom: 1.7px solid #344054;
		transform: translateY(-70%) rotate(45deg);
		pointer-events: none;
	}
	.workspace-tabs {
		height: 54px;
		display: flex;
		align-items: stretch;
		gap: 28px;
		padding: 0 clamp(20px, 4vw, 64px);
		background: #fff;
		border-bottom: 1px solid var(--line);
	}
	.workspace-tabs a {
		display: flex;
		align-items: center;
		position: relative;
		color: #748095;
		text-decoration: none;
		font-size: 13px;
		font-weight: 600;
	}
	.workspace-tabs a:hover {
		color: #3f4a5d;
	}
	.workspace-tabs a.active {
		color: #6653c7;
	}
	.workspace-tabs a.active::after {
		content: '';
		position: absolute;
		left: 0;
		right: 0;
		bottom: -1px;
		height: 2px;
		background: #7561d3;
		border-radius: 2px 2px 0 0;
	}
	.publish-status {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 16px;
		margin: 0 clamp(20px, 4vw, 64px);
		padding: 11px 14px;
		border: 1px solid #b8e4d4;
		border-radius: 10px;
		background: #effaf5;
		color: #17684f;
	}
	.publish-status-copy {
		display: flex;
		align-items: center;
		gap: 9px;
		min-width: 0;
		flex-wrap: wrap;
	}
	.publish-status-copy i {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: #1c9c70;
		box-shadow: 0 0 0 3px #d5f1e5;
	}
	.publish-status-copy i.connecting {
		background: #d59726;
		box-shadow: 0 0 0 3px #faeacb;
	}
	.publish-status-copy strong {
		font-size: 13px;
	}
	.publish-status-copy span {
		color: #407b67;
	}
	.publish-status-copy small {
		color: #71847d;
	}
	.publish-status-copy .publish-error {
		color: #ad3d3d;
	}
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}
	@media (max-width: 700px) {
		.topbar {
			align-items: flex-start;
			flex-direction: column;
			padding: 14px 18px;
			gap: 13px;
		}
		.workspace-actions {
			width: 100%;
			flex-wrap: wrap;
			gap: 7px;
		}
		.channel-picker {
			flex: 1 1 100%;
		}
		.channel-picker select {
			width: 100%;
			max-width: none;
		}
		.workspace-tabs {
			gap: 20px;
			padding: 0 18px;
			overflow-x: auto;
		}
		.publish-status {
			margin: 0 18px;
			align-items: flex-start;
		}
		.publish-status-copy {
			gap: 5px 8px;
		}
		.content {
			padding: 24px 18px 36px;
		}
	}
	.content {
		padding: 34px clamp(22px, 3vw, 48px) 48px;
		max-width: min(1580px, calc(80vw + clamp(44px, 6vw, 96px)));
		margin: 0 auto;
		width: 100%;
	}
	@media (max-width: 760px) {
		.content {
			max-width: 100%;
		}
	}
	.button {
		border: 1px solid var(--accent);
		color: white;
		background: var(--accent);
		border-radius: 8px;
		padding: 9px 14px;
		font-weight: 650;
		font-size: 12px;
		line-height: 1.3;
		box-shadow: 0 1px 2px #1d174010;
		transition:
			background 0.15s,
			border-color 0.15s,
			transform 0.15s;
	}
	.button:hover {
		background: #5845b7;
		border-color: #5845b7;
	}
	.button:active {
		transform: translateY(1px);
	}
	.button:disabled {
		opacity: 0.48;
		cursor: not-allowed;
	}
	/* Shared visual language for route-level components. */
	:global(.content h1) {
		font-size: 30px;
		line-height: 1.2;
		letter-spacing: -0.7px;
		color: #202938;
		margin: 7px 0 7px;
		font-weight: 720;
	}
	:global(.content h2) {
		color: #263143;
		font-size: 16px;
		letter-spacing: -0.2px;
		font-weight: 680;
	}
	:global(.content p) {
		color: #758194;
		font-size: 13px;
		line-height: 1.6;
	}
	:global(.content .eyebrow) {
		font-size: 10px;
		letter-spacing: 1.45px;
		color: #7663c9;
		font-weight: 750;
	}
	:global(.content .panel) {
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content section.panel) {
		background: #fff;
		border-color: #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content aside.panel) {
		display: flex;
		flex-direction: column;
		background: #fff;
		border: 1px solid #e3e8ef;
		border-radius: 13px;
		box-shadow: 0 2px 7px #26354a08;
	}
	:global(.content .button) {
		border-radius: 8px;
		font-size: 12px;
		padding: 9px 14px;
	}
	:global(.content .button.secondary) {
		border-color: #dfe4eb;
		background: #fff;
		color: #4b5668;
	}
	:global(.content input:not([type='checkbox']), .content select) {
		color: #273247;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 38px;
		transition:
			border-color 0.15s,
			box-shadow 0.15s;
	}
	:global(.content input:not([type='checkbox']):focus, .content select:focus) {
		border-color: #9988ed;
		box-shadow: 0 0 0 3px #7663d51a;
		outline: 0;
	}
	:global(.content label) {
		color: #667286;
		font-size: 12px;
		font-weight: 570;
	}
	:global(.content .error) {
		color: #a63242;
		background: #fff2f3;
		border: 1px solid #f4d6da;
		border-radius: 9px;
		padding: 11px 14px;
	}
	:global(.content .empty) {
		color: #7a8596;
		background: #fafbfd;
		border: 1px dashed #dce2ea;
		border-radius: 10px;
		padding: 32px 20px;
		text-align: center;
	}
	:global(.content .form.panel),
	:global(.content .filters.panel) {
		padding: 22px;
		border-radius: 13px;
		gap: 16px;
	}
	:global(.content .form label),
	:global(.content .filters label) {
		display: grid;
		align-content: end;
		gap: 7px;
		font-size: 12px;
		color: #657185;
	}
	:global(
		.content .form input:not([type='checkbox']),
		.content .form select,
		.content .filters input,
		.content .filters select
	) {
		width: 100%;
		min-width: 0;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 40px;
	}
	:global(.content .fields input[type='number']) {
		width: 180px;
		background: #fbfcfd;
		color: #273247;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 9px 11px;
		min-height: 40px;
	}
	:global(.content .cards .card) {
		padding: 18px;
		gap: 9px;
	}
	:global(.content .cards .card small) {
		font-size: 11px;
		color: #778397;
		font-weight: 600;
	}
	:global(.content .cards .card strong) {
		font-size: 25px;
		line-height: 1.2;
		letter-spacing: -0.5px;
		color: #283448;
	}
	:global(.content .cards .card strong.green) {
		color: #16835f;
	}
	:global(.content .cards .card strong.waiting) {
		color: #a66b1e;
	}
	:global(.content .cards .card span) {
		font-size: 11px;
		line-height: 1.45;
		color: #748095;
	}
	:global(.content .stream-row) {
		color: #283448;
		border-bottom-color: #edf0f4;
		padding: 13px 15px;
	}
	:global(.content aside.panel h2) {
		color: #263143;
	}
	:global(.content aside.panel .stream-list) {
		flex: 1;
		border-top-color: #edf0f4;
	}
	:global(.content .video-top select) {
		color: #344054;
		background: #fbfcfd;
		border: 1px solid #dce2ea;
		border-radius: 8px;
		padding: 8px 10px;
		min-height: 36px;
	}
	:global(.content .mode) {
		color: #738095;
		border-radius: 7px;
		padding: 8px 10px;
	}
	:global(.content .mode.chosen) {
		color: #5743b1;
		background: #f0edff;
	}
	:global(.content .notice) {
		border: 1px solid #f1dfbd;
		background: #fff8eb;
		color: #875719;
		padding: 11px 14px;
		border-radius: 9px;
		margin-bottom: 15px;
		font-size: 12px;
	}
	:global(.content .stream-row:hover, .content .stream-row.selected-row) {
		background: #f7f6ff;
	}
	:global(.content .stream-icon) {
		background: #f0edff;
		color: #6955c9;
	}
	:global(.content .stream-copy small) {
		color: #7e899a;
	}
	:global(.content .offline) {
		color: #758195;
	}
	:global(.content .offline.online) {
		color: #16835f;
	}
	:global(.content .count) {
		background: #f1f3f7;
		color: #667286;
	}
	:global(.content .stream-control) {
		background: #f8f9fb;
		border-color: #e5e9ef;
		color: #667286;
	}
	:global(.content .stream-control strong) {
		color: #4f3ca8;
	}
	:global(.content .stream-control small) {
		color: #798496;
	}
	:global(.content .add-row input) {
		background: #fbfcfd;
		color: #273247;
		border-color: #dce2ea;
		min-height: 38px;
	}
	:global(.content .panel-title .eyebrow) {
		color: #7663c9;
	}
	:global(.content .list-empty) {
		display: grid;
		place-items: center;
		min-height: 180px;
		color: #778397;
		text-align: center;
	}
	:global(.content .job) {
		border-top-color: #edf0f4;
	}
	:global(.content .panel h2) {
		color: #263143;
	}
	:global(.content .jobs.panel) {
		padding: 20px;
	}
	:global(.content .title) {
		margin-bottom: 14px;
	}
	:global(.content .title h2) {
		margin: 0;
	}
	:global(.content .job-mark) {
		background: #f0edff;
		color: #6955c9;
	}
	:global(.content .job-main small, .content .progress small) {
		color: #778397;
		font-size: 11px;
	}
	:global(.content .progress > div) {
		background: #edf0f4;
	}
	:global(.content .progress i) {
		background: linear-gradient(90deg, #6b57ca, #9c8be8);
	}
	:global(.content .timeline-head small) {
		color: #778397;
	}
	:global(.content .bucket) {
		background: #f9fafc;
		border-color: #e4e8ef;
		border-radius: 9px;
		padding: 12px;
	}
	:global(.content .bucket:hover) {
		border-color: #9988ed;
		background: #f6f4ff;
	}
	:global(.content .bucket span) {
		font-size: 11px;
		color: #344054;
		font-weight: 620;
	}
	:global(.content .bucket small) {
		font-size: 10px;
		color: #778397;
	}
	:global(.content .bucket i) {
		background: #e9edf3;
	}
	:global(.content .bucket b) {
		background: linear-gradient(90deg, #6955c9, #a595ef);
	}
	:global(.content .legend) {
		color: #687487;
		font-size: 11px;
	}
	:global(.content .range-dot.good) {
		background: #37b987;
	}
	:global(.content .row) {
		border-color: #edf0f4;
	}
	:global(.content .row strong) {
		color: #344054;
	}
	:global(.content .row small) {
		color: #778397;
	}
	:global(.content .row time) {
		color: #687487;
	}
	:global(.content .info p) {
		color: #758194;
	}
	.auth-loading {
		min-height: 100vh;
		display: flex;
		align-items: center;
		justify-content: center;
		gap: 12px;
		color: #687487;
		font-size: 13px;
	}
	.loading-mark {
		width: 36px;
		height: 36px;
		object-fit: contain;
	}
	@media (max-width: 720px) {
		:global(.content h1) {
			font-size: 26px;
		}
	}
</style>
