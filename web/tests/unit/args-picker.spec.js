import { expect } from 'chai';
import { mount, createLocalVue } from '@vue/test-utils';
import Vuetify from 'vuetify';
import ArgsPicker from '@/components/ArgsPicker.vue';

const localVue = createLocalVue();
localVue.use(Vuetify);

describe('ArgsPicker.vue', () => {
  let vuetify;

  // The jsdom the runner sets up is not visual, so it has no
  // requestAnimationFrame. VTextField calls it from its mounted hook to place
  // the label, and without it every mount logs a Vue error.
  before(() => {
    if (!global.requestAnimationFrame) {
      global.requestAnimationFrame = (callback) => setTimeout(callback, 0);
    }
  });

  beforeEach(() => {
    vuetify = new Vuetify();
  });

  // Mounted rather than shallow-mounted: pressing Enter is the point of the
  // test, and a stubbed v-text-field has no input to press a key in -- the
  // handler is bound to the component, which only forwards the listener to the
  // real input it renders.
  function mountPicker() {
    return mount(ArgsPicker, {
      localVue,
      vuetify,
      propsData: { vars: [] },
      mocks: { $t: (k) => k },
      stubs: {
        // v-dialog moves its content to a detached element, out of reach of
        // the wrapper.
        'v-dialog': { template: '<div><slot /></div>' },
      },
    });
  }

  it('saveVar adds the arg and closes the edit dialog', async () => {
    const wrapper = mountPicker();

    wrapper.vm.editVar(null);
    await wrapper.vm.$nextTick();

    const input = wrapper.find('input');
    // Typed rather than assigned: the value has to reach the field itself,
    // because the form validates what the field holds before saving.
    await input.setValue('--check');
    await input.trigger('keydown.enter');
    await wrapper.vm.$nextTick();

    expect(wrapper.emitted().change[0][0]).to.deep.equal(['--check']);
    expect(wrapper.vm.editDialog).to.equal(false);
  });

  // An empty name fails the field's own rule, and saving it would put a bare
  // "--" on the ansible command line.
  it('refuses to save an arg without a name', async () => {
    const wrapper = mountPicker();

    wrapper.vm.editVar(null);
    await wrapper.vm.$nextTick();

    await wrapper.find('input').trigger('keydown.enter');
    await wrapper.vm.$nextTick();

    expect(wrapper.emitted().change).to.equal(undefined);
    expect(wrapper.vm.editDialog).to.equal(true);
  });
});
