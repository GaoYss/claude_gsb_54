<template>
  <el-dialog
    :model-value="modelValue"
    title="退回待处理"
    width="560px"
    @update:model-value="$emit('update:modelValue', $event)"
    @open="syncForm"
  >
    <el-alert
      class="return-alert"
      type="warning"
      :closable="false"
      show-icon
      title="现场判断有误时, 将该维修退回待处理。退回后故障回到待处理, 路灯运行状态随之回落, 之后可重新开工。"
    />

    <el-descriptions v-if="model" :column="1" border size="small" class="repair-summary">
      <el-descriptions-item label="维修单号">{{ model.repair_no }}</el-descriptions-item>
      <el-descriptions-item label="故障单号">{{ model.fault_no }}</el-descriptions-item>
      <el-descriptions-item label="维修人员">{{ model.repairman }}</el-descriptions-item>
    </el-descriptions>

    <el-form ref="formRef" :model="form" :rules="rules" label-width="92px">
      <el-form-item label="退回原因" prop="reason">
        <el-input
          v-model="form.reason"
          type="textarea"
          :rows="3"
          maxlength="512"
          show-word-limit
          placeholder="请说明判断错在哪里、为何退回待处理, 例如: 到场复核为外接电源跳闸, 灯具本身无故障"
        />
      </el-form-item>
      <el-form-item label="退回人" prop="operator">
        <el-input v-model="form.operator" maxlength="64" placeholder="默认为维修人员" />
      </el-form-item>
      <el-form-item label="退回时间" prop="returned_at">
        <el-date-picker
          v-model="form.returned_at"
          type="datetime"
          value-format="YYYY-MM-DD HH:mm:ss"
          placeholder="默认取当前时间"
          style="width: 100%"
        />
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="$emit('update:modelValue', false)">取消</el-button>
      <el-button type="danger" :loading="submitting" @click="handleSubmit">确认退回</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { repairApi } from '@/api/repair'

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  model: { type: Object, default: null },
})

const emit = defineEmits(['update:modelValue', 'saved'])

const formRef = ref(null)
const submitting = ref(false)

const createForm = () => ({
  reason: '',
  operator: '',
  returned_at: '',
})

const form = reactive(createForm())

const rules = {
  reason: [{ required: true, message: '请填写退回原因', trigger: 'blur' }],
}

function syncForm() {
  Object.assign(form, createForm())
  if (props.model) {
    form.operator = props.model.repairman ?? ''
  }
}

async function handleSubmit() {
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    const payload = { ...form }
    if (!payload.operator) {
      delete payload.operator
    }
    if (!payload.returned_at) {
      delete payload.returned_at
    }
    await repairApi.returnToPending(props.model.id, payload)
    ElMessage.success('维修已退回待处理')
    emit('update:modelValue', false)
    emit('saved')
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.return-alert {
  margin-bottom: 16px;
}

.repair-summary {
  margin-bottom: 16px;
}
</style>
