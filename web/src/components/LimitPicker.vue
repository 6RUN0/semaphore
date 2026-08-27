<template>
  <div class="pb-6" style="margin-top: -10px;">
    <div class="d-flex align-center mb-2">
      <div class="subtitle-2">{{ $t('limit') }}</div>
      <v-spacer />
      <v-btn
        icon
        small
        :loading="refreshing"
        :disabled="loading || inventoryId == null"
        :title="$t('refreshHostList')"
        @click="refresh()"
      >
        <v-icon small>mdi-refresh</v-icon>
      </v-btn>
    </div>

    <v-combobox
      v-model="selection"
      :items="[]"
      :label="$t('limit')"
      :hint="$t('limitHint')"
      persistent-hint
      multiple
      small-chips
      deletable-chips
      hide-no-data
      outlined
      dense
      append-icon=""
    />

    <v-alert
      v-if="error"
      type="error"
      dense
      text
      class="mt-3 mb-0"
    >{{ error }}</v-alert>

    <v-alert
      v-else-if="unsupported"
      type="info"
      dense
      text
      class="mt-3 mb-0"
    >
      {{ $t('hostListUnavailable') }}
      <span v-if="snapshot.error" class="d-block mt-1">{{ snapshot.error }}</span>
    </v-alert>

    <v-alert
      v-else-if="snapshotFailed"
      type="warning"
      dense
      text
      class="mt-3 mb-0"
    >{{ $t('hostListFailed') }} {{ snapshot.error }}</v-alert>

    <v-alert
      v-else-if="pendingResolve"
      type="info"
      dense
      text
      class="mt-3 mb-0"
    >
      <v-progress-circular indeterminate size="14" width="2" class="mr-2" />
      {{ $t('hostListResolving') }}
    </v-alert>

    <v-card v-else-if="!loading" outlined class="mt-3">
      <v-row no-gutters>
        <v-col cols="5" class="limit-picker__column">
          <div class="caption text--secondary px-3 pt-2">{{ $t('groups') }}</div>

          <v-list dense class="py-0 limit-picker__list">
            <v-list-item
              v-for="group in groups"
              :key="group.name"
              @click="filterByGroup(group.name)"
              :input-value="activeGroup === group.name"
            >
              <v-list-item-action class="limit-picker__check" @click.stop>
                <v-simple-checkbox
                  :value="isSelected(group.name)"
                  :ripple="false"
                  @input="toggle(group.name)"
                />
              </v-list-item-action>
              <v-list-item-content>
                <v-list-item-title :title="group.name">{{ group.name }}</v-list-item-title>
              </v-list-item-content>
              <span class="caption text--secondary limit-picker__count">
                {{ group.host_count }}
              </span>
            </v-list-item>

            <v-list-item v-if="groups.length === 0">
              <v-list-item-content>
                <v-list-item-title
                  class="caption text--secondary"
                >{{ $t('noGroups') }}</v-list-item-title>
              </v-list-item-content>
            </v-list-item>
          </v-list>
        </v-col>

        <v-divider vertical />

        <v-col cols="7">
          <div class="d-flex align-center px-3 pt-2">
            <div class="caption text--secondary">{{ $t('hosts') }}</div>
            <v-chip
              v-if="activeGroup"
              x-small
              close
              class="ml-2"
              @click:close="filterByGroup(activeGroup)"
            >{{ activeGroup }}</v-chip>
          </div>

          <v-text-field
            v-model="search"
            :placeholder="$t('searchHosts')"
            prepend-inner-icon="mdi-magnify"
            class="px-3 pt-1"
            hide-details
            clearable
            dense
            single-line
          />

          <v-list dense class="py-0 limit-picker__list">
            <v-list-item
              v-for="host in hosts"
              :key="host.name"
              @click="toggle(host.name)"
            >
              <v-list-item-action class="limit-picker__check">
                <v-simple-checkbox
                  :value="isSelected(host.name)"
                  :ripple="false"
                  @input="toggle(host.name)"
                />
              </v-list-item-action>
              <v-list-item-content>
                <v-list-item-title :title="host.name">{{ host.name }}</v-list-item-title>
              </v-list-item-content>
            </v-list-item>

            <v-list-item v-if="hosts.length === 0 && !hostsLoading">
              <v-list-item-content>
                <v-list-item-title
                  class="caption text--secondary"
                >{{ $t('noHosts') }}</v-list-item-title>
              </v-list-item-content>
            </v-list-item>
          </v-list>

          <div v-if="pageIsFull" class="caption text--secondary px-3 pb-2">
            {{ $t('hostListTruncated', { count: hosts.length }) }}
          </div>
        </v-col>
      </v-row>
    </v-card>
  </div>
</template>

<style lang="scss">
.limit-picker__list {
  max-height: 240px;
  overflow-y: auto;

  // Vuetify gives a list item generous side padding and every action slot a
  // 40px minimum. In a column this narrow that leaves a group called "web"
  // rendered as an ellipsis, so both are tightened here.
  .v-list-item {
    padding: 0 8px;
  }

  .v-list-item__title {
    font-size: 0.8125rem;
  }
}

.limit-picker__check {
  min-width: 24px !important;
  margin: 0 6px 0 0 !important;
}

.limit-picker__count {
  flex: 0 0 auto;
  padding-left: 6px;
}

.limit-picker__column {
  overflow: hidden;
}
</style>

<script>
/* eslint-disable import/no-extraneous-dependencies,import/extensions */

import axios from 'axios';

// searchDebounce keeps a keystroke from becoming a request. The host list is
// paged on the server, so every change of the query is a round trip.
const searchDebounce = 250;

// resolvePollInterval is how often a pending resolve is asked about. The
// server answers a pending read from the stored row without touching git, so
// the poll is cheap; the resolve itself usually takes a few seconds.
const resolvePollInterval = 3000;

export default {
  props: {
    projectId: [Number, String],
    // Route parameters arrive as strings; a strict Number here would reject
    // them with a prop warning and blank the picker.
    inventoryId: [Number, String],
    templateId: [Number, String],
    value: Array,
  },

  data() {
    return {
      snapshot: null,
      groups: [],
      hosts: [],
      search: '',
      activeGroup: null,
      loading: false,
      hostsLoading: false,
      refreshing: false,
      error: null,
      searchTimeout: null,
      pollTimeout: null,
      // hostsRequest numbers the host requests in flight so that a slow older
      // response can be told from the current one and dropped.
      hostsRequest: 0,
    };
  },

  computed: {
    selection: {
      get() {
        return this.value || [];
      },
      set(newValue) {
        this.$emit('input', newValue);
      },
    },

    unsupported() {
      return this.snapshot != null && this.snapshot.status === 'unsupported';
    },

    snapshotFailed() {
      return this.snapshot != null && this.snapshot.status === 'error';
    },

    pendingResolve() {
      return this.snapshot != null && this.snapshot.status === 'pending';
    },

    // pageIsFull means the server returned a whole page, so there are probably
    // more hosts than are shown. Saying so is the difference between a list
    // the user can trust and one that quietly hides a host.
    pageIsFull() {
      return this.hosts.length >= this.pageSize;
    },

    pageSize() {
      return 500;
    },
  },

  watch: {
    inventoryId() {
      this.load();
    },

    templateId() {
      this.load();
    },

    search() {
      // load() resets the query as part of its own flow and follows up with
      // loadHosts itself; reacting here too would fire a duplicate request.
      if (this.loading) {
        return;
      }

      clearTimeout(this.searchTimeout);
      this.searchTimeout = setTimeout(() => this.loadHosts(), searchDebounce);
    },
  },

  created() {
    this.load();
  },

  beforeDestroy() {
    clearTimeout(this.searchTimeout);
    clearTimeout(this.pollTimeout);
  },

  methods: {
    isSelected(name) {
      return this.selection.includes(name);
    },

    toggle(name) {
      if (this.isSelected(name)) {
        this.selection = this.selection.filter((item) => item !== name);
      } else {
        this.selection = [...this.selection, name];
      }
    },

    filterByGroup(name) {
      this.activeGroup = this.activeGroup === name ? null : name;
      this.loadHosts();
    },

    snapshotUrl(suffix) {
      return `/api/project/${this.projectId}/inventory/${this.inventoryId}/snapshot${suffix}`;
    },

    // A file inventory that names no repository takes its path from the
    // repository of the template being run, so the template travels with
    // every request rather than only with the first.
    snapshotParams(extra) {
      return {
        template_id: this.templateId || undefined,
        ...extra,
      };
    },

    async load() {
      clearTimeout(this.pollTimeout);

      if (this.inventoryId == null) {
        this.snapshot = null;
        this.groups = [];
        this.hosts = [];
        return;
      }

      this.loading = true;
      this.error = null;
      this.activeGroup = null;
      this.search = '';

      try {
        this.snapshot = (await axios({
          method: 'get',
          url: this.snapshotUrl(''),
          params: this.snapshotParams(),
          responseType: 'json',
        })).data;

        if (this.snapshot.status !== 'ok') {
          this.groups = [];
          this.hosts = [];

          // A pending resolve is a runner working right now; keep asking
          // until it settles one way or the other.
          if (this.snapshot.status === 'pending') {
            this.schedulePoll();
          }

          return;
        }

        this.groups = (await axios({
          method: 'get',
          url: this.snapshotUrl('/groups'),
          params: this.snapshotParams(),
          responseType: 'json',
        })).data;

        await this.loadHosts();
      } catch (err) {
        this.error = err.response?.data?.error || err.message;
      } finally {
        this.loading = false;
      }
    },

    async loadHosts() {
      if (this.inventoryId == null || this.snapshot?.status !== 'ok') {
        return;
      }

      // Typing issues one request per debounce window and nothing orders the
      // responses, so a slow earlier one can land after a newer one and show
      // results for a search the user has already replaced.
      this.hostsRequest += 1;
      const request = this.hostsRequest;

      this.hostsLoading = true;

      try {
        const { data } = await axios({
          method: 'get',
          url: this.snapshotUrl('/hosts'),
          params: this.snapshotParams({
            group: this.activeGroup || undefined,
            search: this.search || undefined,
            limit: this.pageSize,
          }),
          responseType: 'json',
        });

        if (request === this.hostsRequest) {
          this.hosts = data;
        }
      } catch (err) {
        if (request === this.hostsRequest) {
          this.error = err.response?.data?.error || err.message;
        }
      } finally {
        // A newer request owns the spinner from here on, so only the latest
        // one may turn it off.
        if (request === this.hostsRequest) {
          this.hostsLoading = false;
        }
      }
    },

    schedulePoll() {
      clearTimeout(this.pollTimeout);
      this.pollTimeout = setTimeout(() => this.load(), resolvePollInterval);
    },

    async refresh() {
      if (this.inventoryId == null) {
        return;
      }

      this.refreshing = true;
      this.error = null;

      try {
        // 200 carries the rebuilt snapshot; 202 means a resolve task was
        // dispatched to a runner and the snapshot is pending. Either way the
        // body is the state to show.
        this.snapshot = (await axios({
          method: 'post',
          url: this.snapshotUrl('/refresh'),
          params: this.snapshotParams(),
          responseType: 'json',
        })).data;

        if (this.pendingResolve) {
          this.groups = [];
          this.hosts = [];
          this.schedulePoll();
        } else {
          await this.load();
        }
      } catch (err) {
        if (err.response?.status === 403) {
          this.error = this.$t('refreshHostListForbidden');
        } else {
          this.error = err.response?.data?.error || err.message;
        }
      } finally {
        this.refreshing = false;
      }
    },
  },
};
</script>
