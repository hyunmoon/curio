import {LitElement, html, css} from 'https://cdn.jsdelivr.net/gh/lit/dist@3/all/lit-all.min.js';
import RPCCall from '/lib/jsonrpc.mjs';
import { pollRPC } from '/lib/poll.mjs';

class ClusterTasks extends LitElement {
  static get properties() {
    return {
      data: { type: Array },
      showBackgroundTasks: { type: Boolean },
      coalesceEntries: { type: Boolean },
      pendingRenderLimit: { type: Number },
      pendingTotal: { type: Number },
    };
  }

  static get styles() {
    return css`
      th,
      td {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }

      th:nth-child(1),
      td:nth-child(1) {
        width: 8ch;
      }
      th:nth-child(2),
      td:nth-child(2) {
        width: 16ch;
      }
      th:nth-child(3),
      td:nth-child(3) {
        width: 10ch;
      }
      th:nth-child(4),
      td:nth-child(4) {
        width: 10ch;
      }
      th:nth-child(5),
      td:nth-child(5) {
        width: 10ch;
      }
      th:nth-child(6),
      td:nth-child(6) {
        min-width: 20ch;
      }

      .cluster-tasks-root {
        max-width: calc(100vw - 4rem);
        box-sizing: border-box;
      }

      .task-controls {
        display: flex;
        flex-wrap: wrap;
        gap: 0.75rem 1.5rem;
        align-items: center;
        margin-bottom: 1rem;
      }

      .task-controls label {
        display: inline-flex;
        gap: 0.4rem;
        align-items: center;
        white-space: nowrap;
      }

      .pending-limit-input {
        width: 5ch;
      }

      .task-summary {
        margin: 0.75rem 0;
        padding: 0.6rem 0.8rem;
      }

      .task-table-wrap {
        max-width: 100%;
        overflow-x: auto;
      }

      table {
        min-width: 58rem;
      }

      /* Row used to coalesce runs of similar tasks */
      .similar-row > td {
        background: var(--color-form-group-2);
        line-height: 1.2em;
        text-align: center;
        font-style: italic;
      }
    `;
  }

  constructor() {
    super();
    this.data = [];
    this.pendingTotal = 0;
    this.showBackgroundTasks = false;
    this.coalesceEntries = false;
    this.pendingRenderLimit = 30;
    pollRPC(async () => {
      const summary = await RPCCall('ClusterTaskSummary');
      if (Array.isArray(summary)) {
        this.data = summary || [];
        this.pendingTotal = this.data.filter((entry) => !entry.OwnerID).length;
      } else {
        this.data = summary && summary.Tasks ? summary.Tasks : [];
        this.pendingTotal = summary && summary.PendingTotal ? summary.PendingTotal : 0;
      }
    }, 1000);
  }

  toggleShowBackgroundTasks(e) {
    this.showBackgroundTasks = e.target.checked;
  }

  toggleCoalesceEntries(e) {
    this.coalesceEntries = e.target.checked;
  }

  changePendingRenderLimit(e) {
    const parsed = Number.parseInt(e.target.value, 10);
    this.pendingRenderLimit = Number.isFinite(parsed) && parsed >= 0 ? parsed : 30;
  }

  /**
   * Group consecutive entries that share the same SpID, task name, and OwnerID.
   * Returns an array of groups, where each group is an array of entries.
   */
  groupData(data) {
    const groups = [];
    let currentGroup = [];
    let currentKey = null;

    for (const entry of data) {
      // The grouping key is the triplet: [SpID, Name, OwnerID]
      const key = JSON.stringify([entry.SpID, entry.Name, entry.OwnerID]);
      if (key !== currentKey) {
        if (currentGroup.length > 0) {
          groups.push(currentGroup);
        }
        currentGroup = [entry];
        currentKey = key;
      } else {
        currentGroup.push(entry);
      }
    }
    // Push the last group
    if (currentGroup.length > 0) {
      groups.push(currentGroup);
    }

    return groups;
  }

  /**
   * Renders table rows for a group of entries.
   * If coalesce mode is off or a group has <= 3 entries, render all rows.
   * Otherwise, render the first, a "similar tasks" row, then the last.
   */
  renderTableRows(entries) {
    if (!this.coalesceEntries || entries.length <= 3) {
      return entries.map((entry) => this.renderRow(entry));
    }

    const firstEntry = entries[0];
    const lastEntry = entries[entries.length - 1];
    const middleCount = entries.length - 2;

    return html`
      ${this.renderRow(firstEntry)}
      <tr class="similar-row">
        <td colspan="6">${middleCount} similar tasks</td>
      </tr>
      ${this.renderRow(lastEntry)}
    `;
  }

  renderRow(entry) {
    const state = entry.State || (entry.OwnerID ? 'running' : 'queued');

    return html`
      <tr>
        <td>${entry.SpID ? entry.Miner : 'n/a'}</td>
        <td>${entry.Name}</td>
        <td><a href="/pages/task/id/?id=${entry.ID}">${entry.ID}</a></td>
        <td>${entry.SincePostedStr}</td>
        <td>${state}</td>
        <td>
          ${entry.OwnerID
              ? html`<a href="/pages/node_info/?id=${entry.OwnerID}">${entry.Owner}</a>`
              : ''}
        </td>
      </tr>
    `;
  }

  render() {
    // First, filter out background tasks if needed.
    const filtered = this.data.filter(
        (entry) => this.showBackgroundTasks || !entry.Name.startsWith('bg:')
    );

    // Rendering thousands of pending tasks can lock up the browser. Always show
    // running tasks, but cap pending tasks to keep the page responsive.
    const running = filtered.filter((entry) => entry.OwnerID);
    const pending = filtered.filter((entry) => !entry.OwnerID);
    const pendingShown = pending.slice(0, this.pendingRenderLimit);
    const pendingHidden = Math.max(0, pending.length - pendingShown.length);
    const pendingTotal = Math.max(this.pendingTotal || 0, pending.length);
    const renderData = [...running, ...pendingShown];

    let sortedOrOriginal = renderData;

    // In coalesced mode, we sort by [Name -> SpID -> OwnerID]
    // Otherwise, leave data in its default order (e.g., posted time).
    if (this.coalesceEntries) {
      sortedOrOriginal = [...renderData].sort((a, b) => {
        const nameCmp = a.Name.localeCompare(b.Name);
        if (nameCmp !== 0) return nameCmp;
        // If SpID is numeric, do numeric sort, else compare as strings
        const spA = typeof a.SpID === 'number' ? a.SpID : Number.parseInt(a.SpID, 10) || a.SpID;
        const spB = typeof b.SpID === 'number' ? b.SpID : Number.parseInt(b.SpID, 10) || b.SpID;
        const spCmp = spA > spB ? 1 : spA < spB ? -1 : 0;
        if (spCmp !== 0) return spCmp;
        // Compare OwnerIDs (if numeric, do numeric compare; fallback to string)
        const ownerA = typeof a.OwnerID === 'number' ? a.OwnerID : Number.parseInt(a.OwnerID, 10) || a.OwnerID || '';
        const ownerB = typeof b.OwnerID === 'number' ? b.OwnerID : Number.parseInt(b.OwnerID, 10) || b.OwnerID || '';
        const ownerCmp = ownerA > ownerB ? 1 : ownerA < ownerB ? -1 : 0;
        return ownerCmp;
      });
    }

    // If coalescing, group them, otherwise each entry is its own group
    const grouped = this.coalesceEntries
        ? this.groupData(sortedOrOriginal)
        : sortedOrOriginal.map((e) => [e]);

    return html`
      <link
        href="https://cdn.jsdelivr.net/npm/bootstrap@5.1.3/dist/css/bootstrap.min.css"
        rel="stylesheet"
        integrity="sha384-1BmE4kWBq78iYhFldvKuhfTAU6auU8tT94WrHftjDbrCEXSU1oBoqyl2QvZ6jIW3"
        crossorigin="anonymous"
      />
      <link
        rel="stylesheet"
        href="/ux/main.css"
        onload="document.body.style.visibility = 'initial'"
      />

      <div class="cluster-tasks-root">
        <div class="task-controls">
          <label>
            <input
              type="checkbox"
              @change=${this.toggleShowBackgroundTasks}
              ?checked=${this.showBackgroundTasks}
            />
            Show background tasks
          </label>

          <label>
            <input
              type="checkbox"
              @change=${this.toggleCoalesceEntries}
              ?checked=${this.coalesceEntries}
            />
            Coalesce Entries
          </label>

          <label>
            Pending:
            <input
              class="pending-limit-input"
              type="number"
              min="0"
              step="10"
              .value=${String(this.pendingRenderLimit)}
              @change=${this.changePendingRenderLimit}
            />
          </label>
        </div>

        <div class="alert alert-info task-summary" role="alert">
          Running: ${running.length} · Pending: ${pendingTotal > 0 ? html`${pendingShown.length}/${pendingTotal}` : '0'}
        </div>

        <div class="task-table-wrap">
          <table class="table table-dark mt-3">
            <thead>
              <tr>
                <th>SpID</th>
                <th style="min-width: 128px">Task</th>
                <th>ID</th>
                <th>Age</th>
                <th>State</th>
                <th>Owner</th>
              </tr>
            </thead>
            <tbody>
              ${grouped.map((group) => this.renderTableRows(group))}
            </tbody>
          </table>
        </div>
      </div>
    `;
  }
}

customElements.define('cluster-tasks', ClusterTasks);
