document.addEventListener('DOMContentLoaded', () => {
    const statTotal = document.getElementById('stat-total');
    const statMax = document.getElementById('stat-max');
    const feed = document.getElementById('feed');
    const statusBadge = document.getElementById('status-badge');
    const statusText = document.getElementById('status-text');
    const flashOverlay = document.getElementById('flash-overlay');

    // Controls
    const ampInput = document.getElementById('amplitude');
    const ampVal = document.getElementById('amp-val');
    const cooldownInput = document.getElementById('cooldown');
    const cooldownVal = document.getElementById('cooldown-val');
    const speedInput = document.getElementById('speed');
    const speedVal = document.getElementById('speed-val');
    const volScaling = document.getElementById('volume-scaling');
    const pauseToggle = document.getElementById('pause-toggle');
    const packInput = document.getElementById('soundpack');
    const themeSelect = document.getElementById('theme-select');

    let totalSlaps = 0;
    let maxAmp = 0;

    // Format time
    const timeFormatter = new Intl.DateTimeFormat(undefined, {
        hour: '2-digit', minute: '2-digit', second: '2-digit', fractionalSecondDigits: 3
    });

    // Theme Management
    function applyTheme(theme) {
        document.documentElement.classList.remove('theme-light', 'theme-dark');
        if (theme === 'light' || theme === 'dark') {
            document.documentElement.classList.add(`theme-${theme}`);
        }
        localStorage.setItem('spankmac-theme', theme);
    }
    
    const savedTheme = localStorage.getItem('spankmac-theme') || 'auto';
    themeSelect.value = savedTheme;
    applyTheme(savedTheme);
    
    themeSelect.addEventListener('change', (e) => applyTheme(e.target.value));

    function setStatus(online) {
        if (online) {
            statusBadge.classList.add('connected');
            statusText.textContent = 'Listening';
        } else {
            statusBadge.classList.remove('connected');
            statusText.textContent = 'Disconnected';
        }
    }

    // Initialize SSE
    function connectSSE() {
        const evtSource = new EventSource('/events');
        
        evtSource.onopen = () => setStatus(true);
        evtSource.onerror = () => setStatus(false);
        
        evtSource.onmessage = (e) => {
            const data = JSON.parse(e.data);
            handleSlap(data);
        };
    }

    async function fetchStatus() {
        try {
            const res = await fetch('/api/status');
            if (res.ok) {
                const data = await res.json();
                
                if (data.pack) {
                    packInput.value = data.pack;
                }
                
                ampInput.value = data.amplitude;
                ampVal.textContent = data.amplitude.toFixed(2);
                
                cooldownInput.value = data.cooldown;
                cooldownVal.textContent = data.cooldown;
                
                speedInput.value = data.speed;
                speedVal.textContent = data.speed.toFixed(1) + 'x';
                
                volScaling.checked = data.volume_scaling;
                pauseToggle.checked = data.paused;
            }
        } catch (err) {
            console.error("Failed to fetch status", err);
        }
    }

    // Settings Updaters
    async function updateSettings() {
        const body = {
            amplitude: parseFloat(ampInput.value),
            cooldown: parseInt(cooldownInput.value),
            speed: parseFloat(speedInput.value),
            pack: packInput.value
        };
        try {
            await fetch('/api/settings', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            });
        } catch (err) {
            console.error("Failed to update settings", err);
        }
    }

    // Input listeners (local state update)
    ampInput.addEventListener('input', e => ampVal.textContent = parseFloat(e.target.value).toFixed(2));
    cooldownInput.addEventListener('input', e => cooldownVal.textContent = e.target.value);
    speedInput.addEventListener('input', e => speedVal.textContent = parseFloat(e.target.value).toFixed(1) + 'x');

    // Input listeners (server update on release)
    ampInput.addEventListener('change', updateSettings);
    cooldownInput.addEventListener('change', updateSettings);
    speedInput.addEventListener('change', updateSettings);
    packInput.addEventListener('change', updateSettings);

    // Toggles
    volScaling.addEventListener('change', async (e) => {
        await fetch('/api/settings', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ toggle_volume_scaling: true }) // custom field just to toggle
        });
    });

    pauseToggle.addEventListener('change', async (e) => {
        const endpoint = e.target.checked ? '/api/pause' : '/api/resume';
        await fetch(endpoint, { method: 'POST' });
    });

    // Handle incoming slaps
    function handleSlap(data) {
        totalSlaps++;
        if (data.amplitude > maxAmp) {
            maxAmp = data.amplitude;
            statMax.textContent = maxAmp.toFixed(2) + 'g';
        }
        statTotal.textContent = totalSlaps;

        // Visual flash overlay
        flashOverlay.classList.remove('flash-anim');
        void flashOverlay.offsetWidth; // trigger reflow
        flashOverlay.classList.add('flash-anim');

        // Remove empty state if present
        const empty = feed.querySelector('.empty-state');
        if (empty) empty.remove();

        const tsStr = timeFormatter.format(new Date(data.timestamp));
        const filename = data.file.split('/').pop();

        const el = document.createElement('div');
        el.className = `slap-item sev-${data.severity}`;
        
        el.innerHTML = `
            <div class="slap-number">#${data.slapNumber}</div>
            <div class="slap-content">
                <div class="slap-title">
                    <span>Impact Detected</span>
                    <span class="slap-tag">${data.severity}</span>
                </div>
                <div class="slap-file">Played: ${filename}</div>
                <div class="slap-meta">
                    <span>${tsStr}</span>
                    <span class="slap-amp">${data.amplitude.toFixed(4)}g</span>
                </div>
            </div>
        `;

        feed.prepend(el);

        // Keep only last 50 items to avoid DOM overload
        if (feed.children.length > 50) {
            feed.removeChild(feed.lastChild);
        }
    }

    // Boot
    fetchStatus();
    connectSSE();
});
