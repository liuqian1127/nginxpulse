<template>
  <div class="header-toolbar">
    <div v-if="slots.primary" class="toolbar-group toolbar-group-primary">
      <slot name="primary" />
    </div>
    <div v-if="slots.utility" class="toolbar-group toolbar-group-utility">
      <slot name="utility" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { useSlots } from 'vue';

const slots = useSlots();
</script>

