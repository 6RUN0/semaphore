<template>
  <div>
    <v-checkbox
        :input-value="enabled"
        :label="$t('advancedOptions')"
        @change="$emit('update:enabled', !!$event)"
    />

    <HighlightedCard
        v-if="enabled"
        tick-left="80px"
        style="width: 350px;"
    >
      <v-text-field
          :value="checkInterval"
          type="number"
          min="1"
          :label="$t('runnerCheckInterval')"
          :hint="$t('runnerCheckIntervalHint')"
          :rules="[checkIntervalRule]"
          persistent-hint
          dense
          outlined
          @input="$emit('update:check-interval', $event)"
      />
    </HighlightedCard>
  </div>
</template>

<script>
import HighlightedCard from '@/components/HighlightedCard.vue';

// The registration and the token screen offer the same runner settings, so the
// block lives here rather than being repeated in both.
export default {
  components: { HighlightedCard },

  props: {
    enabled: Boolean,
    checkInterval: {
      type: [Number, String],
      default: 1,
    },
  },

  methods: {
    // Number('') and Number(null) are 0, so the positivity check also rejects
    // an emptied field.
    checkIntervalRule(v) {
      const n = Number(v);
      return (Number.isInteger(n) && n > 0) || this.$t('runnerCheckIntervalInvalid');
    },
  },
};
</script>
