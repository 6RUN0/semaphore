import { expect } from 'chai';
import { shallowMount } from '@vue/test-utils';
import Vue from 'vue';
import Vuetify from 'vuetify';

Vue.use(Vuetify);

// The view pulls in ProjectForm, whose import chain ends at
// src/plugins/i18.js, which reads localStorage while the module is still
// being evaluated; node has none unless started with --localstorage-file. The
// view is therefore imported after the stub is in place, which a static
// import could not do.
let Settings;

before(async () => {
  Object.defineProperty(global, 'localStorage', {
    configurable: true,
    value: {
      getItem: () => 'en',
      setItem: () => {},
      removeItem: () => {},
    },
  });

  ({ default: Settings } = await import('@/views/project/Settings.vue'));
});

// See survey-vars.spec.js for why $vuetify is mocked next to the instance.
function mountSettings() {
  const vuetify = new Vuetify();
  return shallowMount(Settings, {
    vuetify,
    propsData: { projectId: 7, projectType: '', systemInfo: {} },
    mocks: { $t: (k) => k, $vuetify: vuetify.framework },
  });
}

describe('project/Settings.vue', () => {
  // Webhook aliases are the secret of the public webhook route; the server
  // leaves them out of a backup unless the download asks for them.
  it('downloads the backup without webhook aliases by default', () => {
    const wrapper = mountSettings();

    expect(wrapper.vm.backupUrl()).to.equal('/api/project/7/backup');
  });

  it('asks for webhook aliases when the option is ticked', async () => {
    const wrapper = mountSettings();

    await wrapper.setData({ backupIncludeIntegrationAliases: true });

    expect(wrapper.vm.backupUrl()).to.equal('/api/project/7/backup?include_integration_aliases=true');
  });
});
