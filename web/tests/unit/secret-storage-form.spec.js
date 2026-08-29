import { expect } from 'chai';
import { shallowMount, createLocalVue } from '@vue/test-utils';
import Vuetify from 'vuetify';

const localVue = createLocalVue();
localVue.use(Vuetify);

// The form pulls in ItemFormBase, and that import chain ends at
// src/plugins/i18.js, which reads localStorage while the module is still being
// evaluated. Node exposes localStorage only when started with
// --localstorage-file, so the read throws before any test runs. The component
// is therefore imported after the stub is in place, which a static import
// could not do: those are hoisted above everything else in the module.
let SecretStorageForm;

before(async () => {
  Object.defineProperty(global, 'localStorage', {
    configurable: true,
    value: {
      // A locale has to come back, otherwise i18.js falls through to
      // navigator.language, which node does not provide either.
      getItem: () => 'en',
      setItem: () => {},
      removeItem: () => {},
    },
  });

  ({ default: SecretStorageForm } = await import('@/components/SecretStorageForm.vue'));
});

// A new storage is filled in locally by getNewItem(), so nothing here reaches
// the network and no endpoint has to be stubbed away.
async function mountForm(itemType) {
  const wrapper = shallowMount(SecretStorageForm, {
    localVue,
    vuetify: new Vuetify(),
    propsData: { itemId: 'new', projectId: 1, itemType },
    mocks: { $t: (k) => k },
  });

  // created() loads the item asynchronously, and the form renders nothing
  // until it is there (`v-if="item != null"`).
  await wrapper.vm.$nextTick();
  await wrapper.vm.$nextTick();

  return wrapper;
}

const tlsControl = '[data-testid="secretStorage-vaultTlsSkipVerify"]';

describe('SecretStorageForm.vue', () => {
  // Skipping certificate verification makes Semaphore accept any certificate
  // from the Vault it holds a project token for. The flag lives in the stored
  // params and the form submits the whole item, so it survives any edit: with
  // no control for it, an active insecure setting is invisible in the UI and
  // cannot be turned back off there. It was dropped once already, while
  // resolving a merge conflict, which is what this pins.
  it('offers the TLS verification control for vault', async () => {
    const wrapper = await mountForm('vault');

    expect(
      wrapper.find(tlsControl).exists(),
      'the vault form must offer the TLS skip-verify control',
    ).to.equal(true);
  });

  // The vault section is shared with openbao, so the control has to be there
  // for both rather than in a vault-only branch.
  it('offers the TLS verification control for openbao', async () => {
    const wrapper = await mountForm('openbao');

    expect(
      wrapper.find(tlsControl).exists(),
      'openbao shares the vault section and its controls',
    ).to.equal(true);
  });

  // Bound to the stored params rather than to a local field: a local one would
  // save nothing and would not show an insecure setting already in the database.
  it('binds the control to the stored flag', async () => {
    const wrapper = await mountForm('vault');

    // $set, because a new storage starts with empty params and a plain
    // assignment would add a property Vue 2 does not watch. The template does
    // the same: v-model on a member expression compiles down to $set.
    wrapper.vm.$set(wrapper.vm.item.params, 'tls_skip_verify', true);
    await wrapper.vm.$nextTick();

    expect(wrapper.find(tlsControl).props('inputValue')).to.equal(true);
  });

  // aws_sm has no vault section at all, and a control leaking into it would
  // offer a setting that storage does not have.
  it('leaves the control out of storages without a vault section', async () => {
    const wrapper = await mountForm('aws_sm');

    expect(wrapper.find(tlsControl).exists()).to.equal(false);
  });
});
