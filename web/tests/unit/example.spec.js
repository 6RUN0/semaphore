import { expect } from 'chai';
import { shallowMount } from '@vue/test-utils';
import YesNoDialog from '@/components/YesNoDialog.vue';

// v-dialog renders its content into a detached element and only while it is
// open, so the real one would put the buttons out of the wrapper's reach. The
// stub renders the slot in place, which is what the assertions below need.
function mountDialog(propsData = {}) {
  return shallowMount(YesNoDialog, {
    propsData: { value: true, ...propsData },
    mocks: { $t: (key) => key },
    stubs: {
      'v-dialog': { template: '<div><slot /></div>' },
    },
  });
}

describe('YesNoDialog.vue', () => {
  it('shows the button titles it was given', () => {
    const wrapper = mountDialog({ noButtonTitle: 'Not now', yesButtonTitle: 'Do it' });

    expect(wrapper.text()).to.include('Not now');
    expect(wrapper.text()).to.include('Do it');
  });

  // Without titles the dialog falls back to the translated defaults, which is
  // how nearly every call site uses it.
  it('falls back to the translated titles', () => {
    const wrapper = mountDialog();

    expect(wrapper.text()).to.include('cancel');
    expect(wrapper.text()).to.include('yes');
  });

  // The caller that suppresses the "no" button must not be left with a dialog
  // that still offers a way to decline.
  it('drops the no button when it is hidden', () => {
    const wrapper = mountDialog({ hideNoButton: true });

    expect(wrapper.text()).to.not.include('cancel');
    expect(wrapper.text()).to.include('yes');
  });
});
