document.addEventListener('DOMContentLoaded', () => {
    // UI Elements
    const tabWrite = document.getElementById('tab-write');
    const tabRead = document.getElementById('tab-read');
    const writeSection = document.getElementById('write-section');
    const readSection = document.getElementById('read-section');
    
    const formCreateURL = document.getElementById('form-create-url');
    const formResolveURL = document.getElementById('form-resolve-url');
    const selectActiveCodes = document.getElementById('select-active-codes');
    const inputShortCode = document.getElementById('input-short-code');
    
    const btnReset = document.getElementById('btn-reset');
    const terminalLogs = document.getElementById('terminal-logs');
    const svgCanvas = document.getElementById('topology-svg');
    
    // Trace elements
    const traceServedBy = document.getElementById('trace-served-by');
    const traceFailover = document.getElementById('trace-failover');
    const tracePath = document.getElementById('trace-path');
    const traceNetLatency = document.getElementById('trace-net-latency');
    const traceDuration = document.getElementById('trace-duration');
    const traceStatus = document.getElementById('trace-status');
    const tracePayload = document.getElementById('trace-payload');

    // Local state
    let activeCodes = new Set();
    let regionsData = {};

    // 1. Tab switching logic
    tabWrite.addEventListener('click', () => {
        tabWrite.classList.add('active');
        tabRead.classList.remove('active');
        writeSection.classList.add('active');
        readSection.classList.remove('active');
    });

    tabRead.addEventListener('click', () => {
        tabRead.classList.add('active');
        tabWrite.classList.remove('active');
        readSection.classList.add('active');
        writeSection.classList.remove('active');
        // Pre-fill read code text field with selected dropdown option if empty
        if (selectActiveCodes.value && !inputShortCode.value) {
            inputShortCode.value = selectActiveCodes.value;
        }
    });

    selectActiveCodes.addEventListener('change', () => {
        inputShortCode.value = selectActiveCodes.value;
    });

    // 2. Poll health stats
    async function updateHealthStats() {
        try {
            const resp = await fetch('/api/regions');
            if (!resp.ok) throw new Error('Router connection failed');
            
            regionsData = await resp.json();
            
            // Update Router Status indicator
            document.getElementById('router-pulse').className = 'pulse-indicator healthy';
            document.getElementById('router-status').textContent = 'ONLINE (PORT 8080)';
            
            // Update region cards
            for (const regionName in regionsData) {
                const data = regionsData[regionName];
                updateRegionCardUI(regionName, data);
            }

            // Draw base network topology lines
            drawTopologyLines();

        } catch (err) {
            document.getElementById('router-pulse').className = 'pulse-indicator';
            document.getElementById('router-status').textContent = 'UNREACHABLE';
            addLogLine(`[SYSTEM ERROR] Failed to fetch regional metrics: ${err.message}`, 'fail-line');
        }
    }

    function updateRegionCardUI(name, data) {
        const card = document.getElementById(`card-${name}`);
        if (!card) return;

        // Card class based on overall health
        card.classList.remove('healthy', 'degraded', 'unhealthy');
        if (!data.healthy) {
            card.classList.add('unhealthy');
        } else if (data.health_detail && data.health_detail.status === 'degraded') {
            card.classList.add('degraded');
        } else {
            card.classList.add('healthy');
        }

        // Components status
        const appEl = document.getElementById(`comp-${name}-app`);
        const redisEl = document.getElementById(`comp-${name}-redis`);
        const dbEl = document.getElementById(`comp-${name}-db`);

        if (!data.healthy) {
            setComponentStatus(appEl, 'DOWN', true);
            setComponentStatus(redisEl, 'DOWN', true);
            setComponentStatus(dbEl, 'DOWN', true);
        } else if (data.health_detail) {
            setComponentStatus(appEl, 'UP', false);
            setComponentStatus(redisEl, data.health_detail.redis, data.health_detail.redis === 'DOWN');
            
            const dbStatus = data.is_primary ? data.health_detail.primary_db : data.health_detail.replica_db;
            setComponentStatus(dbEl, dbStatus, dbStatus === 'DOWN');

            // Wire up Kill button state
            setupKillButton(name, 'all', !data.healthy, appEl.querySelector('.btn-toggle-fail'));
            setupKillButton(name, 'redis', data.health_detail.simulated.redis, redisEl.querySelector('.btn-toggle-fail'));
            
            const dbCompName = data.is_primary ? 'primary_db' : 'replica_db';
            const dbSimulatedFailed = data.is_primary ? data.health_detail.simulated.primary_db : data.health_detail.simulated.replica_db;
            setupKillButton(name, dbCompName, dbSimulatedFailed, dbEl.querySelector('.btn-toggle-fail'));
        }

        // Metrics
        if (data.stats) {
            document.getElementById(`metric-${name}-req`).textContent = data.stats.total_requests || 0;
            document.getElementById(`metric-${name}-ch`).textContent = data.stats.cache_hits || 0;
            document.getElementById(`metric-${name}-rh`).textContent = data.stats.replica_hits || 0;
            
            const fallbackEl = document.getElementById(`metric-${name}-fb`);
            if (fallbackEl) {
                fallbackEl.textContent = data.stats.primary_fallback || 0;
            }
        }
    }

    function setComponentStatus(el, status, isDown) {
        if (!el) return;
        const statusSpan = el.querySelector('.comp-status');
        statusSpan.textContent = status;
        statusSpan.className = `comp-status ${isDown ? 'status-down' : 'status-up'}`;
    }

    function setupKillButton(region, component, isFailed, btn) {
        if (!btn) return;
        btn.textContent = isFailed ? 'RESTORE' : 'KILL';
        if (isFailed) {
            btn.classList.add('failing');
        } else {
            btn.classList.remove('failing');
        }

        // Remove old event listener
        btn.onclick = async (e) => {
            e.stopPropagation();
            const newStatus = isFailed ? 'up' : 'down';
            btn.disabled = true;
            try {
                // If killing the whole app server, we trigger an HTTP fail to simulate the App going offline
                // For simulator, killing "all" component maps to changing the router health check mapping or stopping port.
                // In our model, we simulate killing the app by telling the router or app to toggle failure
                let compToFail = component;
                if (component === 'all') {
                    // Simulating entire app server failure
                    // We toggle "all" which sets redis, replica_db, and primary_db down
                    compToFail = 'all';
                }

                const resp = await fetch('/api/simulate/fail', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        region: region,
                        component: compToFail,
                        status: newStatus
                    })
                });

                if (!resp.ok) throw new Error('Simulation endpoint failed');
                
                addLogLine(`[SIMULATION] Set ${component.toUpperCase()} in ${region.toUpperCase()} to ${newStatus.toUpperCase()}`, newStatus === 'down' ? 'fail-line' : 'system-line');
                
                await updateHealthStats();
            } catch (err) {
                addLogLine(`[SYSTEM ERROR] Failed to toggle component status: ${err.message}`, 'fail-line');
            } finally {
                btn.disabled = false;
            }
        };
    }

    // 3. Draw connection lines dynamically using coordinates relative to topology container
    function drawTopologyLines() {
        const container = document.querySelector('.topology-container');
        if (!container || !svgCanvas) return;
        
        const cRect = container.getBoundingClientRect();
        
        const client = document.getElementById('node-client');
        const router = document.getElementById('node-router');
        const east = document.getElementById('card-us-east');
        const west = document.getElementById('card-us-west');
        const eu = document.getElementById('card-eu-west');
        
        if (!client || !router || !east || !west || !eu) return;
        
        // Helper to get center of element relative to container
        const getCenter = (el) => {
            const r = el.getBoundingClientRect();
            return {
                x: r.left - cRect.left + r.width / 2,
                y: r.top - cRect.top + r.height / 2
            };
        };

        const cl = getCenter(client);
        const rt = getCenter(router);
        const rEast = getCenter(east);
        const rWest = getCenter(west);
        const rEU = getCenter(eu);

        // Build SVG paths
        let svgHTML = `
            <!-- Client -> Router connection -->
            <path id="path-client-router" class="flow-line" d="M ${cl.x} ${cl.y} L ${rt.x} ${rt.y}" />
            
            <!-- Router -> Region App connections -->
            <path id="path-router-us-east" class="flow-line" d="M ${rt.x} ${rt.y} L ${rEast.x} ${rEast.y - 40}" />
            <path id="path-router-us-west" class="flow-line" d="M ${rt.x} ${rt.y} L ${rWest.x} ${rWest.y - 40}" />
            <path id="path-router-eu-west" class="flow-line" d="M ${rt.x} ${rt.y} L ${rEU.x} ${rEU.y - 40}" />
            
            <!-- Read Replicas DB replication sync links (US-East -> US-West, EU-West) -->
            <path id="repl-us-east-us-west" class="flow-line replication-path" d="M ${rEast.x + 30} ${rEast.y + 40} C ${rEast.x + 100} ${rEast.y + 110}, ${rWest.x - 100} ${rWest.y + 110}, ${rWest.x - 30} ${rWest.y + 40}" marker-end="url(#arrow-replication)" />
            <path id="repl-us-east-eu-west" class="flow-line replication-path" d="M ${rEast.x + 50} ${rEast.y + 40} C ${rEast.x + 200} ${rEast.y + 160}, ${rEU.x - 200} ${rEU.y + 160}, ${rEU.x - 50} ${rEU.y + 40}" marker-end="url(#arrow-replication)" />
        `;
        
        svgCanvas.innerHTML = svgHTML;
    }

    // 4. Trigger UI animation for request
    function animateTraffic(clientRegion, servedRegion, isFailover, source, dbFallback) {
        drawTopologyLines(); // Recalculate positions
        
        const clientRouterPath = document.getElementById('path-client-router');
        const routerRegionPath = document.getElementById(`path-router-${servedRegion}`);
        const fallbackPath = document.getElementById(`repl-us-east-${clientRegion}`); // replication path used backward for fallback

        if (!clientRouterPath || !routerRegionPath) return;

        // Reset any previous animations
        clientRouterPath.className.baseVal = "flow-line";
        document.querySelectorAll('.flow-line').forEach(el => {
            if (!el.id.startsWith('repl-')) {
                el.className.baseVal = "flow-line";
            }
        });

        // Set active path
        clientRouterPath.className.baseVal = "flow-line active-path";
        routerRegionPath.className.baseVal = `flow-line active-path ${isFailover ? 'failover-path' : ''}`;

        // If fallback to US-East primary DB happens, animate link from replica app back to us-east primary
        if (dbFallback && clientRegion !== 'us-east') {
            const replPath = document.getElementById(`repl-us-east-${clientRegion}`);
            if (replPath) {
                // Flash the database fallback path
                replPath.className.baseVal = "flow-line active-path failover-path";
                setTimeout(() => {
                    replPath.className.baseVal = "flow-line replication-path";
                }, 3000);
            }
        }

        // Clean up active client-router lines after animation
        setTimeout(() => {
            clientRouterPath.className.baseVal = "flow-line";
            routerRegionPath.className.baseVal = "flow-line";
        }, 2000);
    }

    // 5. Submit write request
    formCreateURL.addEventListener('submit', async (e) => {
        e.preventDefault();
        
        const region = document.getElementById('input-target-region').value;
        const longURL = document.getElementById('input-long-url').value;
        const customCode = document.getElementById('input-custom-code').value.trim();
        
        addLogLine(`[REQUEST] Initiating WRITE operation from client in ${region.toUpperCase()}...`, 'system-line');

        try {
            const resp = await fetch(`/api/request?client_region=${region}&path=/urls`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    url: longURL,
                    code: customCode
                })
            });

            const result = await resp.json();

            // Update Transaction UI
            updateTraceUI(result, 'POST /urls');
            
            if (result.status_code === 201) {
                const body = JSON.parse(result.response_body);
                const code = body.code;
                
                // Add code to selector
                addActiveCode(code);
                
                addLogLine(`[WRITE SUCCESS] Code: ${code} -> ${longURL}. Latency: ${result.total_latency_ms}ms (Network: ${result.network_latency || 0}ms)`, 'write-line');
                
                // Animate path to US-East (Writes always serve from primary)
                animateTraffic(region, 'us-east', region !== 'us-east', 'Primary DB', false);
                
                // Reset custom code input
                document.getElementById('input-custom-code').value = '';
            } else {
                const errBody = JSON.parse(result.response_body || '{}');
                const errMsg = errBody.error || `HTTP ${result.status_code}`;
                addLogLine(`[WRITE FAILURE] ${errMsg}. (Served by: ${result.served_by || 'Unknown'})`, 'fail-line');
            }

            await updateHealthStats();

        } catch (err) {
            addLogLine(`[SYSTEM ERROR] Write request failed: ${err.message}`, 'fail-line');
        }
    });

    // 6. Submit read request
    formResolveURL.addEventListener('submit', async (e) => {
        e.preventDefault();
        
        const region = document.getElementById('input-client-region').value;
        const code = inputShortCode.value.trim() || selectActiveCodes.value;
        
        if (!code) {
            alert('Please select or enter a short code');
            return;
        }

        addLogLine(`[REQUEST] Resolving code '${code}' from client in ${region.toUpperCase()}...`, 'system-line');

        try {
            const resp = await fetch(`/api/request?client_region=${region}&path=/${code}`, {
                method: 'GET'
            });

            const result = await resp.json();

            // Update Transaction UI
            updateTraceUI(result, `GET /${code}`);
            
            if (result.status_code === 302) {
                const destination = result.redirect_url;
                const source = result.source || 'Database';
                const servedBy = result.served_by || region;
                const isFailover = result.failover === 'true';
                const dbFallback = result.db_fallback === 'true';

                let logMsg = `[READ REDIRECT] Resolved '${code}' -> ${destination}. Served by: ${servedBy.toUpperCase()} via ${source}.`;
                if (isFailover) logMsg += ' [ROUTING FAILOVER TRIGGERED]';
                if (dbFallback) logMsg += ' [CROSS-REGION DATABASE FALLBACK]';
                logMsg += ` Latency: ${result.total_latency_ms}ms (Net: ${result.network_latency || 0}ms)`;

                addLogLine(logMsg, 'read-line');
                
                // Animate traffic path
                animateTraffic(region, servedBy, isFailover, source, dbFallback);
            } else {
                const errBody = JSON.parse(result.response_body || '{}');
                const errMsg = errBody.error || `HTTP ${result.status_code}`;
                addLogLine(`[READ FAILURE] Code '${code}' resolution failed: ${errMsg}. (Served by: ${result.served_by || 'Unknown'})`, 'fail-line');
            }

            await updateHealthStats();

        } catch (err) {
            addLogLine(`[SYSTEM ERROR] Read resolution failed: ${err.message}`, 'fail-line');
        }
    });

    // 7. Reset Simulation
    btnReset.addEventListener('click', async () => {
        if (!confirm('Are you sure you want to flush all codes, caches, and restore all component services?')) return;
        
        btnReset.disabled = true;
        try {
            const resp = await fetch('/api/simulate/reset', { method: 'POST' });
            if (!resp.ok) throw new Error('Reset failed');
            
            // Clear UI lists
            activeCodes.clear();
            selectActiveCodes.innerHTML = '<option value="" disabled selected>Select generated code...</option>';
            inputShortCode.value = '';
            
            // Log to terminal
            addLogLine('[SYSTEM RESET] Cleared all caches, restored all regions to UP, wiped mock database tables.', 'system-line');
            
            // Clear Trace
            clearTraceUI();
            
            await updateHealthStats();
        } catch (err) {
            addLogLine(`[SYSTEM ERROR] Reset failed: ${err.message}`, 'fail-line');
        } finally {
            btnReset.disabled = false;
        }
    });

    // --- Auxiliary Helpers ---

    function addActiveCode(code) {
        if (activeCodes.has(code)) return;
        activeCodes.add(code);
        
        const option = document.createElement('option');
        option.value = code;
        option.textContent = code;
        selectActiveCodes.appendChild(option);
    }

    function addLogLine(text, className) {
        const line = document.createElement('div');
        line.className = `log-line ${className || ''}`;
        
        const timestamp = new Date().toLocaleTimeString();
        line.textContent = `[${timestamp}] ${text}`;
        
        terminalLogs.appendChild(line);
        terminalLogs.scrollTop = terminalLogs.scrollHeight;
    }

    function updateTraceUI(res, command) {
        traceServedBy.textContent = (res.served_by || 'Unknown').toUpperCase();
        traceServedBy.className = 'val font-mono ' + (res.served_by === 'us-east' ? 'text-cyan' : 'highlight');
        
        traceFailover.textContent = res.failover === 'true' ? 'YES' : 'NO';
        traceFailover.className = 'val ' + (res.failover === 'true' ? 'status-down' : 'status-up');
        
        // Format path
        let pathStr = res.source || '-';
        if (res.db_fallback === 'true') {
            pathStr += ' (DB Fallback)';
        }
        tracePath.textContent = pathStr;
        tracePath.className = 'val ' + (res.db_fallback === 'true' ? 'status-down' : 'status-up');

        traceNetLatency.textContent = res.network_latency ? `${res.network_latency} ms` : '0 ms';
        traceDuration.textContent = `${res.total_latency_ms} ms`;
        
        traceStatus.textContent = res.status_code;
        traceStatus.className = 'val font-mono ' + (res.status_code >= 200 && res.status_code < 400 ? 'status-up' : 'status-down');

        if (res.status_code === 302) {
            tracePayload.textContent = `302 Redirect ➔ ${res.redirect_url}`;
        } else if (res.response_body) {
            tracePayload.textContent = res.response_body;
        } else {
            tracePayload.textContent = '-';
        }
    }

    function clearTraceUI() {
        traceServedBy.textContent = '-';
        traceServedBy.className = 'val font-mono';
        traceFailover.textContent = '-';
        traceFailover.className = 'val';
        tracePath.textContent = '-';
        tracePath.className = 'val';
        traceNetLatency.textContent = '-';
        traceDuration.textContent = '-';
        traceStatus.textContent = '-';
        traceStatus.className = 'val font-mono';
        tracePayload.textContent = '-';
    }

    // Handle screen resize to redraw lines
    window.addEventListener('resize', drawTopologyLines);

    // Initial setup
    updateHealthStats();
    
    // Regular health checker updates
    setInterval(updateHealthStats, 2000);
});
