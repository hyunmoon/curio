import {LitElement, html, css} from 'https://cdn.jsdelivr.net/gh/lit/dist@3/all/lit-all.min.js';
import RPCCall from '/lib/jsonrpc.mjs';
import { pollRPC } from '/lib/poll.mjs';
import {groupConsecutiveTasks} from '/cluster-tasks-grouping.mjs';

class ClusterTasks extends LitElement {
  static get properties() {
    return {
      data: { type: Array },
      showBackgroundTasks: { type: Boolean },
      coalesceEntries: { type: Boolean },
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
    this.showBackgroundTasks = false;
    this.coalesceEntries = true; // Default-enabled coalesce checkbox
    pollRPC(async () => {
      this.data = (await RPCCall('ClusterTaskSummary')) || [];
    }, 1000);
  }

  toggleShowBackgroundTasks(e) {
    this.showBackgroundTasks = e.target.checked;
  }

  toggleCoalesceEntries(e) {
    this.coalesceEntries = e.target.checked;
  }

  /**
   * Group consecutive entries that share the same SpID, task name, and OwnerID.
   * Returns an array of groups, where each group is an array of entries.
   */
  groupData(data) {
    return groupConsecutiveTasks(data);
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
    return html`
      <tr>
        <td>${entry.SpID ? entry.Miner : 'n/a'}</td>
        <td>${entry.Name}</td>
        <td><a href="/pages/task/id/?id=${entry.ID}">${entry.ID}</a></td>
        <td>${entry.State || (entry.OwnerID ? 'running' : 'pending')}</td>
        <td>${entry.Age || entry.SincePostedStr}</td>
        <td>
          ${entry.OwnerID
              ? html`<a href="/pages/node_info/?id=${entry.OwnerID}">${entry.Owner}</a>`
              : ''}
        </td>
      </tr>
    `;
  }

  render() {
    // First, filter out background tasks if needed
    const filtered = this.data.filter(
        (entry) => this.showBackgroundTasks || !entry.Name.startsWith('bg:')
    );

    // If coalescing, group them, otherwise each entry is its own group
    const grouped = this.coalesceEntries
        ? this.groupData(filtered)
        : filtered.map((e) => [e]);

    return html`
      <link rel="stylesheet" href="/ux/vendor/bootstrap.min.css">
      <link
        rel="stylesheet"
        href="/ux/main.css"
        onload="document.body.style.visibility = 'initial'"
      />

      <!-- Toggle for showing background tasks -->
      <label>
        <input
          type="checkbox"
          @change=${this.toggleShowBackgroundTasks}
          ?checked=${this.showBackgroundTasks}
        />
        Show background tasks
      </label>

      <!-- Toggle for coalescing entries -->
      <label style="margin-left: 1em;">
        <input
          type="checkbox"
          @change=${this.toggleCoalesceEntries}
          ?checked=${this.coalesceEntries}
        />
        Coalesce Entries
      </label>

      <table class="table table-dark mt-3">
        <thead>
          <tr>
            <th>SpID</th>
            <th style="min-width: 128px">Task</th>
            <th>ID</th>
            <th>State</th>
            <th>Age</th>
            <th>Owner</th>
          </tr>
        </thead>
        <tbody>
          ${grouped.map((group) => this.renderTableRows(group))}
        </tbody>
      </table>
    `;
  }
}

customElements.define('cluster-tasks', ClusterTasks);
