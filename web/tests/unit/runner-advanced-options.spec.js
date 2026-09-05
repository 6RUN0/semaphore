import { expect } from 'chai';
import { shallowMount, createLocalVue } from '@vue/test-utils';
import Vuetify from 'vuetify';
import HighlightedCard from '@/components/HighlightedCard.vue';
import RunnerAdvancedOptions from '@/components/RunnerAdvancedOptions.vue';

const localVue = createLocalVue();
localVue.use(Vuetify);

// $t returns the key, so an assertion on a rendered label fails as soon as the
// label is hardcoded instead of translated.
function mountOptions(propsData = {}) {
  return shallowMount(RunnerAdvancedOptions, {
    localVue,
    vuetify: new Vuetify(),
    propsData: {
      enabled: false,
      checkInterval: 1,
      ...propsData,
    },
    mocks: { $t: (k) => k },
  });
}

describe('RunnerAdvancedOptions.vue', () => {
  it('translates the checkbox label', () => {
    const wrapper = mountOptions();

    const checkbox = wrapper.findComponent({ name: 'v-checkbox' });

    expect(checkbox.props('label')).to.equal('advancedOptions');
  });

  it('hides the settings until the box is checked', () => {
    const wrapper = mountOptions();

    expect(wrapper.findComponent(HighlightedCard).exists()).to.equal(false);
  });

  it('shows the settings when the box is checked', () => {
    const wrapper = mountOptions({ enabled: true });

    expect(wrapper.findComponent(HighlightedCard).exists()).to.equal(true);
  });

  it('reports the checkbox state to the parent', () => {
    const wrapper = mountOptions();

    wrapper.findComponent({ name: 'v-checkbox' }).vm.$emit('change', true);

    expect(wrapper.emitted()['update:enabled'][0][0]).to.equal(true);
  });

  it('reports an edited interval to the parent', () => {
    const wrapper = mountOptions({ enabled: true });

    wrapper.findComponent({ name: 'v-text-field' }).vm.$emit('input', '30');

    expect(wrapper.emitted()['update:check-interval'][0][0]).to.equal('30');
  });

  // The interval is interpolated into the snippets the operator copies, and a
  // runner refuses a config it cannot parse, so anything but a whole positive
  // number of seconds has to be rejected at the field.
  describe('checkIntervalRule', () => {
    const accepted = [1, '1', 5, '600'];
    const rejected = [0, -1, '', ' ', 'abc', 1.5, '2.5', null];

    accepted.forEach((value) => {
      it(`accepts ${JSON.stringify(value)}`, () => {
        expect(mountOptions().vm.checkIntervalRule(value)).to.equal(true);
      });
    });

    rejected.forEach((value) => {
      it(`rejects ${JSON.stringify(value)}`, () => {
        expect(mountOptions().vm.checkIntervalRule(value))
          .to.equal('runnerCheckIntervalInvalid');
      });
    });
  });
});
