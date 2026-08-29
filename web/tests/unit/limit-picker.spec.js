import { expect } from 'chai';
import { shallowMount, createLocalVue } from '@vue/test-utils';
import Vuetify from 'vuetify';
import axios from 'axios';
import LimitPicker from '@/components/LimitPicker.vue';

const localVue = createLocalVue();
localVue.use(Vuetify);

// Mounting without an inventory keeps load() and loadHosts() from reaching the
// network: both return immediately when there is nothing to load, so no method
// has to be stubbed away.
function mountPicker(propsData = {}) {
  return shallowMount(LimitPicker, {
    localVue,
    vuetify: new Vuetify(),
    propsData: {
      projectId: 1,
      inventoryId: null,
      value: [],
      ...propsData,
    },
    mocks: { $t: (k) => k },
  });
}

describe('LimitPicker.vue', () => {
  it('adds a name to the selection', () => {
    const wrapper = mountPicker();

    wrapper.vm.toggle('web01');

    expect(wrapper.emitted().input[0][0]).to.deep.equal(['web01']);
  });

  it('removes a name that is already selected', () => {
    const wrapper = mountPicker({ value: ['web01', 'db01'] });

    wrapper.vm.toggle('web01');

    expect(wrapper.emitted().input[0][0]).to.deep.equal(['db01']);
  });

  // Selecting a group has to put the group name into --limit rather than
  // expand it: that is what ansible resolves at run time, and it keeps the
  // value stable when the inventory later gains a host.
  it('selects a group by its name rather than by its hosts', async () => {
    const wrapper = mountPicker();
    await wrapper.setData({
      snapshot: { status: 'ok' },
      groups: [{ name: 'web', host_count: 4 }],
      hosts: [{ name: 'web01' }, { name: 'web02' }],
    });

    wrapper.vm.toggle('web');

    expect(wrapper.emitted().input[0][0]).to.deep.equal(['web']);
  });

  it('reports which names are selected', () => {
    const wrapper = mountPicker({ value: ['web'] });

    expect(wrapper.vm.isSelected('web')).to.equal(true);
    expect(wrapper.vm.isSelected('db')).to.equal(false);
  });

  it('treats a missing value as an empty selection', () => {
    const wrapper = mountPicker({ value: undefined });

    expect(wrapper.vm.selection).to.deep.equal([]);
    expect(wrapper.vm.isSelected('web')).to.equal(false);
  });

  it('clears the group filter when the same group is picked twice', () => {
    const wrapper = mountPicker();

    wrapper.vm.filterByGroup('web');
    expect(wrapper.vm.activeGroup).to.equal('web');

    wrapper.vm.filterByGroup('web');
    expect(wrapper.vm.activeGroup).to.equal(null);
  });

  it('switches the group filter to another group', () => {
    const wrapper = mountPicker();

    wrapper.vm.filterByGroup('web');
    wrapper.vm.filterByGroup('db');

    expect(wrapper.vm.activeGroup).to.equal('db');
  });

  it('recognises an inventory whose hosts can not be listed', async () => {
    const wrapper = mountPicker();
    await wrapper.setData({ snapshot: { status: 'unsupported' } });

    expect(wrapper.vm.unsupported).to.equal(true);
    expect(wrapper.vm.snapshotFailed).to.equal(false);
  });

  it('recognises a failed synchronisation', async () => {
    const wrapper = mountPicker();
    await wrapper.setData({ snapshot: { status: 'error', error: 'broken range' } });

    expect(wrapper.vm.snapshotFailed).to.equal(true);
    expect(wrapper.vm.unsupported).to.equal(false);
    expect(wrapper.vm.snapshot.error).to.equal('broken range');
  });

  // A full page means the server had more hosts to give, and saying so is what
  // keeps a user from concluding that a missing host does not exist.
  it('detects that the host page is full', async () => {
    const wrapper = mountPicker();

    await wrapper.setData({ hosts: new Array(wrapper.vm.pageSize).fill({ name: 'h' }) });
    expect(wrapper.vm.pageIsFull).to.equal(true);

    await wrapper.setData({ hosts: [{ name: 'h' }] });
    expect(wrapper.vm.pageIsFull).to.equal(false);
  });

  // A file inventory that names no repository takes its path from the
  // repository of the template being run, so the template has to ride along on
  // every snapshot request rather than only the first.
  it('sends the template with every request when it has one', () => {
    const wrapper = mountPicker({ templateId: 12 });

    expect(wrapper.vm.snapshotParams()).to.deep.equal({ template_id: 12 });
    expect(wrapper.vm.snapshotParams({ group: 'web' })).to.deep.equal({
      template_id: 12,
      group: 'web',
    });
  });

  it('omits the template when there is none', () => {
    const wrapper = mountPicker();

    expect(wrapper.vm.snapshotParams().template_id).to.equal(undefined);
  });

  it('builds snapshot urls under the inventory', () => {
    const wrapper = mountPicker({ projectId: 7, inventoryId: 9 });

    expect(wrapper.vm.snapshotUrl('')).to.equal('/api/project/7/inventory/9/snapshot');
    expect(wrapper.vm.snapshotUrl('/hosts')).to.equal('/api/project/7/inventory/9/snapshot/hosts');
  });
  // A pending snapshot is a runner working right now: the picker shows the
  // resolving state instead of empty lists, and keeps asking until it settles.
  it('reports a pending resolve', async () => {
    const wrapper = mountPicker();
    await wrapper.setData({ snapshot: { status: 'pending', task_id: 5 } });

    expect(wrapper.vm.pendingResolve).to.equal(true);
    expect(wrapper.vm.unsupported).to.equal(false);
    expect(wrapper.vm.snapshotFailed).to.equal(false);
  });

  it('schedules a poll for a pending resolve', () => {
    const wrapper = mountPicker();

    wrapper.vm.schedulePoll();

    expect(wrapper.vm.pollTimeout).to.not.equal(null);
    wrapper.destroy();
  });

  // A resolve whose task no runner picks up stays pending until the server
  // expires the claim half an hour later. At a fixed interval an open tab
  // would ask about it hundreds of times, once per open tab.
  it('stretches the interval between polls', () => {
    const wrapper = mountPicker();

    const intervals = [];
    for (let i = 0; i < 12; i += 1) {
      intervals.push(wrapper.vm.pollInterval);
      wrapper.vm.schedulePoll();
    }

    expect(intervals[1]).to.be.above(intervals[0]);
    expect(intervals[intervals.length - 1]).to.be.at.most(30000);
    expect(intervals[intervals.length - 1]).to.be.above(intervals[1]);
    wrapper.destroy();
  });

  // Past the claim's own lifetime the server settles it, so there is nothing
  // left for the poll to observe.
  it('gives up polling once the claim can no longer be pending', () => {
    const wrapper = mountPicker();

    wrapper.setData({ pollDeadline: Date.now() - 1 });
    wrapper.vm.schedulePoll();

    expect(wrapper.vm.pollTimeout).to.equal(null);
    expect(wrapper.vm.error).to.equal('hostListResolveTimedOut');
    wrapper.destroy();
  });

  // The group list is capped by the server like the host list, but it has no
  // search to narrow it with: without the notice a group the user cannot see
  // looks like a group that does not exist.
  it('detects that the group page is full', async () => {
    const wrapper = mountPicker();

    await wrapper.setData({ groups: [{ name: 'web', host_count: 1 }] });
    expect(wrapper.vm.groupPageIsFull).to.equal(false);

    await wrapper.setData({
      groups: Array.from({ length: 500 }, (unused, i) => ({ name: `g${i}`, host_count: 1 })),
    });
    expect(wrapper.vm.groupPageIsFull).to.equal(true);
  });

  // Route parameters arrive as strings; the picker must accept them rather
  // than reject the prop and blank itself.
  it('accepts a string inventory id', () => {
    const wrapper = mountPicker({ projectId: 7, inventoryId: '9' });

    expect(wrapper.vm.snapshotUrl('')).to.equal('/api/project/7/inventory/9/snapshot');
  });

  // Typing issues one request per debounce window and nothing orders the
  // responses, so a slow earlier one can land after a newer one and put back
  // the results of a search the user has already replaced.
  describe('overlapping host requests', () => {
    let pending = [];
    let savedAdapter;

    // The component and this file import the same axios instance, so replacing
    // the default adapter is enough to hold every request open and answer them
    // in whatever order the test needs.
    beforeEach(() => {
      pending = [];
      savedAdapter = axios.defaults.adapter;
      axios.defaults.adapter = (config) => new Promise((resolve, reject) => {
        pending.push({ config, resolve, reject });
      });
    });

    afterEach(() => {
      axios.defaults.adapter = savedAdapter;
    });

    // created() loads the snapshot too, and axios reaches the adapter only on
    // a later microtask, so the host requests are picked out by url and waited
    // for rather than indexed into whatever happens to be pending.
    function hostRequests() {
      return pending.filter((request) => request.config.url.endsWith('/hosts'));
    }

    async function waitForHostRequests(count) {
      for (let attempt = 0; attempt < 50 && hostRequests().length < count; attempt += 1) {
        // eslint-disable-next-line no-await-in-loop
        await new Promise((resolve) => { setTimeout(resolve, 0); });
      }

      expect(hostRequests().length).to.equal(count);
    }

    function answer(index, data) {
      const request = hostRequests()[index];

      request.resolve({
        data,
        status: 200,
        statusText: 'OK',
        headers: {},
        config: request.config,
      });
    }

    async function pickerWithHosts() {
      const wrapper = mountPicker({ inventoryId: 9 });
      await wrapper.setData({ snapshot: { status: 'ok' } });

      return wrapper;
    }

    it('ignores a host response that a newer request has overtaken', async () => {
      const wrapper = await pickerWithHosts();

      const stale = wrapper.vm.loadHosts();
      const fresh = wrapper.vm.loadHosts();
      await waitForHostRequests(2);

      answer(1, [{ name: 'fresh01' }]);
      await fresh;

      answer(0, [{ name: 'stale01' }]);
      await stale;

      expect(wrapper.vm.hosts).to.deep.equal([{ name: 'fresh01' }]);
      expect(wrapper.vm.hostsLoading).to.equal(false);
    });

    it('keeps the spinner up while a newer request is still in flight', async () => {
      const wrapper = await pickerWithHosts();

      const stale = wrapper.vm.loadHosts();
      const fresh = wrapper.vm.loadHosts();
      await waitForHostRequests(2);

      answer(0, [{ name: 'stale01' }]);
      await stale;

      expect(wrapper.vm.hostsLoading).to.equal(true);

      answer(1, [{ name: 'fresh01' }]);
      await fresh;

      expect(wrapper.vm.hostsLoading).to.equal(false);
      expect(wrapper.vm.hosts).to.deep.equal([{ name: 'fresh01' }]);
    });

    // An error from an overtaken request must not raise a banner about a
    // search the user has already moved on from.
    it('ignores an error from an overtaken request', async () => {
      const wrapper = await pickerWithHosts();

      const stale = wrapper.vm.loadHosts();
      const fresh = wrapper.vm.loadHosts();
      await waitForHostRequests(2);

      answer(1, [{ name: 'fresh01' }]);
      await fresh;

      hostRequests()[0].reject(new Error('connection reset'));
      await stale;

      expect(wrapper.vm.error).to.equal(null);
      expect(wrapper.vm.hosts).to.deep.equal([{ name: 'fresh01' }]);
    });
  });

  // Both timers outlive the component otherwise: the picker is destroyed
  // whenever the task dialog closes, and a poll firing afterwards asks about an
  // inventory nobody is looking at and writes to a component that is gone.
  it('drops both timers when it is destroyed', (done) => {
    const wrapper = mountPicker();

    let fired = false;
    wrapper.vm.searchTimeout = setTimeout(() => { fired = true; }, 5);
    wrapper.vm.pollTimeout = setTimeout(() => { fired = true; }, 5);

    wrapper.destroy();

    setTimeout(() => {
      expect(fired).to.equal(false);
      done();
    }, 30);
  });

  // Typing is one request per debounce window, not one per keystroke: every
  // request costs the server a snapshot freshness check.
  it('collapses a burst of typing into one host request', async () => {
    const wrapper = mountPicker({ inventoryId: 9 });

    // created() has a load() of its own in flight; the watcher stands down
    // while that runs, which is what the next test covers.
    await wrapper.setData({ snapshot: { status: 'ok' }, loading: false });

    let requests = 0;
    wrapper.vm.loadHosts = () => { requests += 1; };

    await wrapper.setData({ search: 'w' });
    await wrapper.setData({ search: 'we' });
    await wrapper.setData({ search: 'web' });

    expect(requests).to.equal(0);

    await new Promise((resolve) => { setTimeout(resolve, 400); });

    expect(requests).to.equal(1);
    wrapper.destroy();
  });

  // load() clears the search as part of its own flow and calls loadHosts
  // itself; the watcher reacting to that clearing too would double every load.
  it('does not react to the search its own load reset', async () => {
    const wrapper = mountPicker({ inventoryId: 9 });

    let requests = 0;
    wrapper.vm.loadHosts = () => { requests += 1; };

    await wrapper.setData({ loading: true, search: 'web' });

    expect(wrapper.vm.searchTimeout).to.equal(null, 'no debounce timer may be armed');

    await new Promise((resolve) => { setTimeout(resolve, 400); });

    expect(requests).to.equal(0);
    wrapper.destroy();
  });

  // The picker stays mounted while the task dialog has no inventory selected
  // yet, and the snapshot routes need one in their path.
  it('clears what it shows when the inventory goes away', async () => {
    const wrapper = mountPicker();

    await wrapper.setData({
      snapshot: { status: 'ok' },
      groups: [{ name: 'web', host_count: 1 }],
      hosts: [{ name: 'web01' }],
    });

    await wrapper.vm.load();

    expect(wrapper.vm.snapshot).to.equal(null);
    expect(wrapper.vm.groups).to.deep.equal([]);
    expect(wrapper.vm.hosts).to.deep.equal([]);
  });
});
