/*
 * Local disk tool for Rockchip parameter.txt based partitioning.
 * No libusb dependency - writes directly to block devices (e.g. /dev/sda).
 *
 * Usage:
 *   rklocaltool gpt     <device> <parameter.txt>
 *   rklocaltool prm     <device> <parameter.txt>
 *   rklocaltool wl      <device> <offset_sectors> <image_file>
 *   rklocaltool wlx     <device> <parameter.txt> <partition_name> <image_file>
 *   rklocaltool fmt     <device> <parameter.txt> <partition_name>
 *   rklocaltool all     <device> <parameter.txt> [image_dir] [--format]
 *   rklocaltool ppt     <device>
 */

#include "RKLocalTool.h"

static unsigned int crc32table_le[] = {
    0x00000000, 0x77073096, 0xee0e612c, 0x990951ba, 0x076dc419, 0x706af48f,
    0xe963a535, 0x9e6495a3, 0x0edb8832, 0x79dcb8a4, 0xe0d5e91e, 0x97d2d988,
    0x09b64c2b, 0x7eb17cbd, 0xe7b82d07, 0x90bf1d91, 0x1db71064, 0x6ab020f2,
    0xf3b97148, 0x84be41de, 0x1adad47d, 0x6ddde4eb, 0xf4d4b551, 0x83d385c7,
    0x136c9856, 0x646ba8c0, 0xfd62f97a, 0x8a65c9ec, 0x14015c4f, 0x63066cd9,
    0xfa0f3d63, 0x8d080df5, 0x3b6e20c8, 0x4c69105e, 0xd56041e4, 0xa2677172,
    0x3c03e4d1, 0x4b04d447, 0xd20d85fd, 0xa50ab56b, 0x35b5a8fa, 0x42b2986c,
    0xdbbbc9d6, 0xacbcf940, 0x32d86ce3, 0x45df5c75, 0xdcd60dcf, 0xabd13d59,
    0x26d930ac, 0x51de003a, 0xc8d75180, 0xbfd06116, 0x21b4f4b5, 0x56b3c423,
    0xcfba9599, 0xb8bda50f, 0x2802b89e, 0x5f058808, 0xc60cd9b2, 0xb10be924,
    0x2f6f7c87, 0x58684c11, 0xc1611dab, 0xb6662d3d, 0x76dc4190, 0x01db7106,
    0x98d220bc, 0xefd5102a, 0x71b18589, 0x06b6b51f, 0x9fbfe4a5, 0xe8b8d433,
    0x7807c9a2, 0x0f00f934, 0x9609a88e, 0xe10e9818, 0x7f6a0dbb, 0x086d3d2d,
    0x91646c97, 0xe6635c01, 0x6b6b51f4, 0x1c6c6162, 0x856530d8, 0xf262004e,
    0x6c0695ed, 0x1b01a57b, 0x8208f4c1, 0xf50fc457, 0x65b0d9c6, 0x12b7e950,
    0x8bbeb8ea, 0xfcb9887c, 0x62dd1ddf, 0x15da2d49, 0x8cd37cf3, 0xfbd44c65,
    0x4db26158, 0x3ab551ce, 0xa3bc0074, 0xd4bb30e2, 0x4adfa541, 0x3dd895d7,
    0xa4d1c46d, 0xd3d6f4fb, 0x4369e96a, 0x346ed9fc, 0xad678846, 0xda60b8d0,
    0x44042d73, 0x33031de5, 0xaa0a4c5f, 0xdd0d7cc9, 0x5005713c, 0x270241aa,
    0xbe0b1010, 0xc90c2086, 0x5768b525, 0x206f85b3, 0xb966d409, 0xce61e49f,
    0x5edef90e, 0x29d9c998, 0xb0d09822, 0xc7d7a8b4, 0x59b33d17, 0x2eb40d81,
    0xb7bd5c3b, 0xc0ba6cad, 0xedb88320, 0x9abfb3b6, 0x03b6e20c, 0x74b1d29a,
    0xead54739, 0x9dd277af, 0x04db2615, 0x73dc1683, 0xe3630b12, 0x94643b84,
    0x0d6d6a3e, 0x7a6a5aa8, 0xe40ecf0b, 0x9309ff9d, 0x0a00ae27, 0x7d079eb1,
    0xf00f9344, 0x8708a3d2, 0x1e01f268, 0x6906c2fe, 0xf762575d, 0x806567cb,
    0x196c3671, 0x6e6b06e7, 0xfed41b76, 0x89d32be0, 0x10da7a5a, 0x67dd4acc,
    0xf9b9df6f, 0x8ebeeff9, 0x17b7be43, 0x60b08ed5, 0xd6d6a3e8, 0xa1d1937e,
    0x38d8c2c4, 0x4fdff252, 0xd1bb67f1, 0xa6bc5767, 0x3fb506dd, 0x48b2364b,
    0xd80d2bda, 0xaf0a1b4c, 0x36034af6, 0x41047a60, 0xdf60efc3, 0xa867df55,
    0x316e8eef, 0x4669be79, 0xcb61b38c, 0xbc66831a, 0x256fd2a0, 0x5268e236,
    0xcc0c7795, 0xbb0b4703, 0x220216b9, 0x5505262f, 0xc5ba3bbe, 0xb2bd0b28,
    0x2bb45a92, 0x5cb36a04, 0xc2d7ffa7, 0xb5d0cf31, 0x2cd99e8b, 0x5bdeae1d,
    0x9b64c2b0, 0xec63f226, 0x756aa39c, 0x026d930a, 0x9c0906a9, 0xeb0e363f,
    0x72076785, 0x05005713, 0x95bf4a82, 0xe2b87a14, 0x7bb12bae, 0x0cb61b38,
    0x92d28e9b, 0xe5d5be0d, 0x7cdcefb7, 0x0bdbdf21, 0x86d3d2d4, 0xf1d4e242,
    0x68ddb3f8, 0x1fda836e, 0x81be16cd, 0xf6b9265b, 0x6fb077e1, 0x18b74777,
    0x88085ae6, 0xff0f6a70, 0x66063bca, 0x11010b5c, 0x8f659eff, 0xf862ae69,
    0x616bffd3, 0x166ccf45, 0xa00ae278, 0xd70dd2ee, 0x4e048354, 0x3903b3c2,
    0xa7672661, 0xd06016f7, 0x4969474d, 0x3e6e77db, 0xaed16a4a, 0xd9d65adc,
    0x40df0b66, 0x37d83bf0, 0xa9bcae53, 0xdebb9ec5, 0x47b2cf7f, 0x30b5ffe9,
    0xbdbdf21c, 0xcabac28a, 0x53b39330, 0x24b4a3a6, 0xbad03605, 0xcdd70693,
    0x54de5729, 0x23d967bf, 0xb3667a2e, 0xc4614ab8, 0x5d681b02, 0x2a6f2b94,
    0xb40bbe37, 0xc30c8ea1, 0x5a05df1b, 0x2d02ef8d
};

unsigned int crc32_le(unsigned int crc, unsigned char *p, unsigned int len)
{
    crc = crc ^ 0xFFFFFFFF;
    for (unsigned int i = 0; i < len; i++)
        crc = (crc >> 8) ^ crc32table_le[(crc ^ p[i]) & 0xFF];
    return crc ^ 0xFFFFFFFF;
}

void gen_rand_uuid(unsigned char *uuid_bin)
{
    efi_guid_t id;
    unsigned int *ptr = (unsigned int *)&id;
    unsigned int i;
    for (i = 0; i < sizeof(id) / sizeof(*ptr); i++)
        ptr[i] = rand();
    id.uuid.time_hi_and_version = (id.uuid.time_hi_and_version & 0x0FFF) | 0x4000;
    id.uuid.clock_seq_hi_and_reserved = id.uuid.clock_seq_hi_and_reserved | 0x80;
    memcpy(uuid_bin, id.raw, sizeof(id));
}

static unsigned int be32(unsigned int x)
{
    return ((x & 0xff) << 24) | ((x & 0xff00) << 8) |
           ((x & 0xff0000) >> 8) | ((x & 0xff000000) >> 24);
}

static unsigned short be16(unsigned short x)
{
    return ((x & 0xff) << 8) | ((x & 0xff00) >> 8);
}

void string_to_uuid(string strUUid, char *uuid)
{
    unsigned int i;
    char value;
    memset(uuid, 0, 16);
    for (i = 0; i < strUUid.size(); i++) {
        value = 0;
        if ((strUUid[i] >= '0') && (strUUid[i] <= '9'))
            value = strUUid[i] - '0';
        if ((strUUid[i] >= 'a') && (strUUid[i] <= 'f'))
            value = strUUid[i] - 'a' + 10;
        if ((strUUid[i] >= 'A') && (strUUid[i] <= 'F'))
            value = strUUid[i] - 'A' + 10;
        if ((i % 2) == 0)
            uuid[i / 2] += (value << 4);
        else
            uuid[i / 2] += value;
    }
    unsigned int *p32;
    unsigned short *p16;
    p32 = (unsigned int *)uuid;
    *p32 = be32(*p32);
    p16 = (unsigned short *)(uuid + 4);
    *p16 = be16(*p16);
    p16 = (unsigned short *)(uuid + 6);
    *p16 = be16(*p16);
}

int find_config_item(CONFIG_ITEM_VECTOR &vecItems, const char *pszName)
{
    unsigned int i;
    for (i = 0; i < vecItems.size(); i++) {
        if (strcasecmp(pszName, vecItems[i].szItemName) == 0)
            return i;
    }
    return -1;
}

static bool ParsePartitionInfo(string &strPartInfo, string &strName, UINT &uiOffset, UINT &uiLen)
{
    string::size_type pos, prevPos;
    string strOffset, strLen;
    int iCount;
    prevPos = pos = 0;
    if (strPartInfo.size() <= 0)
        return false;
    pos = strPartInfo.find('@');
    if (pos == string::npos)
        return false;
    strLen = strPartInfo.substr(prevPos, pos - prevPos);
    strLen.erase(0, strLen.find_first_not_of(" "));
    strLen.erase(strLen.find_last_not_of(" ") + 1);
    if (strchr(strLen.c_str(), '-')) {
        uiLen = 0xFFFFFFFF;
    } else {
        iCount = sscanf(strLen.c_str(), "0x%x", &uiLen);
        if (iCount != 1)
            return false;
    }
    prevPos = pos + 1;
    pos = strPartInfo.find('(', prevPos);
    if (pos == string::npos)
        return false;
    strOffset = strPartInfo.substr(prevPos, pos - prevPos);
    strOffset.erase(0, strOffset.find_first_not_of(" "));
    strOffset.erase(strOffset.find_last_not_of(" ") + 1);
    iCount = sscanf(strOffset.c_str(), "0x%x", &uiOffset);
    if (iCount != 1)
        return false;
    prevPos = pos + 1;
    pos = strPartInfo.find(')', prevPos);
    if (pos == string::npos)
        return false;
    strName = strPartInfo.substr(prevPos, pos - prevPos);
    strName.erase(0, strName.find_first_not_of(" "));
    strName.erase(strName.find_last_not_of(" ") + 1);
    return true;
}

static bool ParseUuidInfo(string &strUuidInfo, string &strName, string &strUUid)
{
    string::size_type pos(0);
    if (strUuidInfo.size() <= 0)
        return false;
    pos = strUuidInfo.find('=');
    if (pos == string::npos)
        return false;
    strName = strUuidInfo.substr(0, pos);
    strName.erase(0, strName.find_first_not_of(" "));
    strName.erase(strName.find_last_not_of(" ") + 1);
    strUUid = strUuidInfo.substr(pos + 1);
    strUUid.erase(0, strUUid.find_first_not_of(" "));
    strUUid.erase(strUUid.find_last_not_of(" ") + 1);
    while (true) {
        pos = 0;
        if ((pos = strUUid.find("-")) != string::npos)
            strUUid.replace(pos, 1, "");
        else
            break;
    }
    if (strUUid.size() != 32)
        return false;
    return true;
}

bool parse_parameter(const char *pParameter, PARAM_ITEM_VECTOR &vecItem, CONFIG_ITEM_VECTOR &vecUuidItem)
{
    stringstream paramStream(pParameter);
    bool bRet, bFind = false;
    string strLine, strPartition, strPartInfo, strPartName, strUUid;
    string::size_type line_size, pos, posColon, posComma;
    UINT uiPartOffset, uiPartSize;
    STRUCT_PARAM_ITEM item;
    STRUCT_CONFIG_ITEM uuid_item;
    vecItem.clear();
    vecUuidItem.clear();
    while (!paramStream.eof()) {
        getline(paramStream, strLine);
        line_size = strLine.size();
        if (line_size == 0)
            continue;
        if (strLine[line_size - 1] == '\r')
            strLine = strLine.substr(0, line_size - 1);
        strLine.erase(0, strLine.find_first_not_of(" "));
        strLine.erase(strLine.find_last_not_of(" ") + 1);
        if (strLine.size() == 0)
            continue;
        if (strLine[0] == '#')
            continue;
        pos = strLine.find("uuid:");
        if (pos != string::npos) {
            strPartInfo = strLine.substr(pos + 5);
            bRet = ParseUuidInfo(strPartInfo, strPartName, strUUid);
            if (bRet) {
                strcpy(uuid_item.szItemName, strPartName.c_str());
                string_to_uuid(strUUid, uuid_item.szItemValue);
                vecUuidItem.push_back(uuid_item);
            }
            continue;
        }
        pos = strLine.find("mtdparts");
        if (pos == string::npos)
            continue;
        bFind = true;
        posColon = strLine.find(':', pos);
        if (posColon == string::npos)
            continue;
        strPartition = strLine.substr(posColon + 1);
        pos = 0;
        posComma = strPartition.find(',', pos);
        while (posComma != string::npos) {
            strPartInfo = strPartition.substr(pos, posComma - pos);
            bRet = ParsePartitionInfo(strPartInfo, strPartName, uiPartOffset, uiPartSize);
            if (bRet) {
                strcpy(item.szItemName, strPartName.c_str());
                item.uiItemOffset = uiPartOffset;
                item.uiItemSize = uiPartSize;
                vecItem.push_back(item);
            }
            pos = posComma + 1;
            posComma = strPartition.find(',', pos);
        }
        strPartInfo = strPartition.substr(pos);
        if (strPartInfo.size() > 0) {
            bRet = ParsePartitionInfo(strPartInfo, strPartName, uiPartOffset, uiPartSize);
            if (bRet) {
                strcpy(item.szItemName, strPartName.c_str());
                item.uiItemOffset = uiPartOffset;
                item.uiItemSize = uiPartSize;
                vecItem.push_back(item);
            }
        }
    }
    return bFind;
}

bool parse_parameter_file(const char *pParamFile, PARAM_ITEM_VECTOR &vecItem, CONFIG_ITEM_VECTOR &vecUuidItem)
{
    FILE *file = fopen(pParamFile, "rb");
    if (!file) {
        printf("parse_parameter_file: can't open file: %s (err=%d)\n", pParamFile, errno);
        return false;
    }
    int iFileSize;
    fseek(file, 0, SEEK_END);
    iFileSize = ftell(file);
    fseek(file, 0, SEEK_SET);
    char *pParamBuf = new char[iFileSize + 1];
    if (!pParamBuf) {
        fclose(file);
        return false;
    }
    memset(pParamBuf, 0, iFileSize + 1);
    int iRead = fread(pParamBuf, 1, iFileSize, file);
    fclose(file);
    if (iRead != iFileSize) {
        delete[] pParamBuf;
        return false;
    }
    bool bRet = parse_parameter(pParamBuf, vecItem, vecUuidItem);
    delete[] pParamBuf;
    return bRet;
}

bool parse_config_file(const char *pConfigFile, CONFIG_ITEM_VECTOR &vecItem)
{
    FILE *file = fopen(pConfigFile, "rb");
    if (!file)
        return false;
    int iFileSize;
    fseek(file, 0, SEEK_END);
    iFileSize = ftell(file);
    fseek(file, 0, SEEK_SET);
    char *pBuf = new char[iFileSize + 1];
    if (!pBuf) {
        fclose(file);
        return false;
    }
    memset(pBuf, 0, iFileSize + 1);
    int iRead = fread(pBuf, 1, iFileSize, file);
    fclose(file);
    if (iRead != iFileSize) {
        delete[] pBuf;
        return false;
    }
    stringstream configStream(pBuf);
    string strLine, strItemName, strItemValue;
    string::size_type pos;
    STRUCT_CONFIG_ITEM item;
    vecItem.clear();
    while (!configStream.eof()) {
        getline(configStream, strLine);
        if (strLine.size() == 0)
            continue;
        if (strLine[strLine.size() - 1] == '\r')
            strLine = strLine.substr(0, strLine.size() - 1);
        strLine.erase(0, strLine.find_first_not_of(" "));
        strLine.erase(strLine.find_last_not_of(" ") + 1);
        if (strLine.size() == 0 || strLine[0] == '#')
            continue;
        pos = strLine.find("=");
        if (pos == string::npos)
            continue;
        strItemName = strLine.substr(0, pos);
        strItemValue = strLine.substr(pos + 1);
        strItemName.erase(0, strItemName.find_first_not_of(" "));
        strItemName.erase(strItemName.find_last_not_of(" ") + 1);
        strItemValue.erase(0, strItemValue.find_first_not_of(" "));
        strItemValue.erase(strItemValue.find_last_not_of(" ") + 1);
        if ((strItemName.size() > 0) && (strItemValue.size() > 0)) {
            strcpy(item.szItemName, strItemName.c_str());
            strcpy(item.szItemValue, strItemValue.c_str());
            vecItem.push_back(item);
        }
    }
    delete[] pBuf;
    return true;
}

void create_gpt_buffer(u8 *gpt, PARAM_ITEM_VECTOR &vecParts, CONFIG_ITEM_VECTOR &vecUuid, u64 diskSectors)
{
    legacy_mbr *mbr = (legacy_mbr *)gpt;
    gpt_header *gptHead = (gpt_header *)(gpt + SECTOR_SIZE);
    gpt_entry *gptEntry = (gpt_entry *)(gpt + 2 * SECTOR_SIZE);
    u32 i, j;
    int pos;
    string strPartName;
    string::size_type colonPos;

    memset(gpt, 0, SECTOR_SIZE);
    mbr->signature = MSDOS_MBR_SIGNATURE;
    mbr->partition_record[0].sys_ind = EFI_PMBR_OSTYPE_EFI_GPT;
    mbr->partition_record[0].start_sect = 1;
    mbr->partition_record[0].nr_sects = (u32)-1;

    memset(gpt + SECTOR_SIZE, 0, SECTOR_SIZE);
    gptHead->signature = GPT_HEADER_SIGNATURE;
    gptHead->revision = GPT_HEADER_REVISION_V1;
    gptHead->header_size = sizeof(gpt_header);
    gptHead->my_lba = 1;
    gptHead->alternate_lba = diskSectors - 1;
    gptHead->first_usable_lba = 34;
    gptHead->last_usable_lba = diskSectors - 34;
    gptHead->partition_entry_lba = 2;
    gptHead->num_partition_entries = GPT_ENTRY_NUMBERS;
    gptHead->sizeof_partition_entry = GPT_ENTRY_SIZE;
    gptHead->header_crc32 = 0;
    gptHead->partition_entry_array_crc32 = 0;
    gen_rand_uuid(gptHead->disk_guid.raw);

    memset(gpt + 2 * SECTOR_SIZE, 0, 32 * SECTOR_SIZE);
    for (i = 0; i < vecParts.size(); i++) {
        gen_rand_uuid(gptEntry->partition_type_guid.raw);
        gen_rand_uuid(gptEntry->unique_partition_guid.raw);
        gptEntry->starting_lba = vecParts[i].uiItemOffset;
        gptEntry->ending_lba = gptEntry->starting_lba + vecParts[i].uiItemSize - 1;
        gptEntry->attributes.raw = 0;
        strPartName = vecParts[i].szItemName;
        colonPos = strPartName.find_first_of(':');
        if (colonPos != string::npos) {
            if (strPartName.find("bootable") != string::npos)
                gptEntry->attributes.raw = PART_PROPERTY_BOOTABLE;
            if (strPartName.find("grow") != string::npos)
                gptEntry->ending_lba = diskSectors - 34;
            strPartName = strPartName.substr(0, colonPos);
            vecParts[i].szItemName[strPartName.size()] = 0;
        }
        for (j = 0; j < strlen(vecParts[i].szItemName); j++)
            gptEntry->partition_name[j] = vecParts[i].szItemName[j];
        if ((pos = find_config_item(vecUuid, vecParts[i].szItemName)) != -1)
            memcpy(gptEntry->unique_partition_guid.raw, vecUuid[pos].szItemValue, 16);
        gptEntry++;
    }

    gptHead->partition_entry_array_crc32 = crc32_le(0, gpt + 2 * SECTOR_SIZE, GPT_ENTRY_SIZE * GPT_ENTRY_NUMBERS);
    gptHead->header_crc32 = crc32_le(0, gpt + SECTOR_SIZE, sizeof(gpt_header));
}

void prepare_gpt_backup(u8 *master, u8 *backup)
{
    gpt_header *gptMasterHead = (gpt_header *)(master + SECTOR_SIZE);
    gpt_header *gptBackupHead = (gpt_header *)(backup + 32 * SECTOR_SIZE);
    u32 calc_crc32;
    u64 val;

    val = gptMasterHead->my_lba;
    gptBackupHead->my_lba = gptMasterHead->alternate_lba;
    gptBackupHead->alternate_lba = val;
    gptBackupHead->partition_entry_lba = gptMasterHead->last_usable_lba + 1;
    gptBackupHead->header_crc32 = 0;
    calc_crc32 = crc32_le(0, (unsigned char *)gptBackupHead, gptMasterHead->header_size);
    gptBackupHead->header_crc32 = calc_crc32;
}

void update_gpt_disksize(u8 *master, u8 *backup, u32 total_sector)
{
    gpt_header *gptMasterHead = (gpt_header *)(master + SECTOR_SIZE);
    gpt_entry *gptLastPartEntry = NULL;
    u32 i;
    u64 old_disksize;
    u8 zerobuf[GPT_ENTRY_SIZE];

    memset(zerobuf, 0, GPT_ENTRY_SIZE);
    old_disksize = gptMasterHead->alternate_lba + 1;
    for (i = 0; i < gptMasterHead->num_partition_entries; i++) {
        gptLastPartEntry = (gpt_entry *)(master + 2 * SECTOR_SIZE + i * GPT_ENTRY_SIZE);
        if (memcmp(zerobuf, (u8 *)gptLastPartEntry, GPT_ENTRY_SIZE) == 0)
            break;
    }
    i--;
    gptLastPartEntry = (gpt_entry *)(master + 2 * SECTOR_SIZE + i * sizeof(gpt_entry));

    gptMasterHead->alternate_lba = total_sector - 1;
    gptMasterHead->last_usable_lba = total_sector - 34;

    if (gptLastPartEntry->ending_lba == (old_disksize - 34)) {
        gptLastPartEntry->ending_lba = total_sector - 34;
        gptMasterHead->partition_entry_array_crc32 = crc32_le(0, master + 2 * SECTOR_SIZE, GPT_ENTRY_SIZE * GPT_ENTRY_NUMBERS);
    }
    gptMasterHead->header_crc32 = 0;
    gptMasterHead->header_crc32 = crc32_le(0, master + SECTOR_SIZE, sizeof(gpt_header));
    memcpy(backup, master + 2 * SECTOR_SIZE, GPT_ENTRY_SIZE * GPT_ENTRY_NUMBERS);
    memcpy(backup + GPT_ENTRY_SIZE * GPT_ENTRY_NUMBERS, master + SECTOR_SIZE, SECTOR_SIZE);
    prepare_gpt_backup(master, backup);
}

u64 get_disk_sectors(const char *device)
{
    int fd = open(device, O_RDONLY);
    if (fd < 0) {
        printf("get_disk_sectors: cannot open %s (err=%d)\n", device, errno);
        return 0;
    }
    u64 size_bytes = 0;
    if (ioctl(fd, BLKGETSIZE64, &size_bytes) < 0) {
        unsigned long sectors = 0;
        if (ioctl(fd, BLKGETSIZE, &sectors) < 0) {
            printf("get_disk_sectors: ioctl BLKGETSIZE failed (err=%d)\n", errno);
            close(fd);
            return 0;
        }
        close(fd);
        return (u64)sectors;
    }
    close(fd);
    return size_bytes / SECTOR_SIZE;
}

bool write_sectors(int fd, u64 lba, u32 count, const u8 *buf)
{
    off64_t offset = (off64_t)lba * SECTOR_SIZE;
    ssize_t total = (ssize_t)count * SECTOR_SIZE;
    ssize_t written = 0;
    while (written < total) {
        ssize_t ret = pwrite64(fd, buf + written, total - written, offset + written);
        if (ret < 0) {
            if (errno == EINTR)
                continue;
            printf("write_sectors: pwrite failed at lba=%llu (err=%d)\n", (unsigned long long)lba, errno);
            return false;
        }
        if (ret == 0)
            break;
        written += ret;
    }
    return (written == total);
}

bool read_sectors(int fd, u64 lba, u32 count, u8 *buf)
{
    off64_t offset = (off64_t)lba * SECTOR_SIZE;
    ssize_t total = (ssize_t)count * SECTOR_SIZE;
    ssize_t nread = 0;
    while (nread < total) {
        ssize_t ret = pread64(fd, buf + nread, total - nread, offset + nread);
        if (ret < 0) {
            if (errno == EINTR)
                continue;
            printf("read_sectors: pread failed at lba=%llu (err=%d)\n", (unsigned long long)lba, errno);
            return false;
        }
        if (ret == 0)
            break;
        nread += ret;
    }
    return (nread == total);
}

static bool make_param_buffer(const char *pParamFile, u8 *&pParamData, UINT &paramSize)
{
    FILE *file = fopen(pParamFile, "rb");
    if (!file) {
        printf("make_param_buffer: can't open %s (err=%d)\n", pParamFile, errno);
        return false;
    }
    int iFileSize;
    fseek(file, 0, SEEK_END);
    iFileSize = ftell(file);
    fseek(file, 0, SEEK_SET);
    u8 *pBuf = new u8[iFileSize + 12];
    if (!pBuf) {
        fclose(file);
        return false;
    }
    memset(pBuf, 0, iFileSize + 12);
    *(UINT *)(pBuf) = PARAM_MAGIC;
    int iRead = fread(pBuf + 8, 1, iFileSize, file);
    fclose(file);
    if (iRead != iFileSize) {
        delete[] pBuf;
        return false;
    }
    *(UINT *)(pBuf + 4) = iFileSize;
    *(UINT *)(pBuf + 8 + iFileSize) = crc32_le(0, pBuf + 8, iFileSize);
    pParamData = pBuf;
    paramSize = iFileSize + 12;
    return true;
}

bool write_gpt_to_disk(const char *device, const char *paramFile)
{
    u64 diskSectors = get_disk_sectors(device);
    if (diskSectors == 0) {
        printf("Failed to get disk size for %s\n", device);
        return false;
    }
    printf("Disk %s: %llu sectors (%llu MB)\n", device,
           (unsigned long long)diskSectors, (unsigned long long)(diskSectors / 2048));

    PARAM_ITEM_VECTOR vecItems;
    CONFIG_ITEM_VECTOR vecUuid;
    bool bRet;

    if (strstr(paramFile, ".img") || strstr(paramFile, ".gpt")) {
        u8 master_gpt[34 * SECTOR_SIZE];
        u8 backup_gpt[33 * SECTOR_SIZE];
        FILE *file = fopen(paramFile, "rb");
        if (!file) {
            printf("Cannot open GPT image: %s\n", paramFile);
            return false;
        }
        fseek(file, 0, SEEK_END);
        int sz = ftell(file);
        fseek(file, 0, SEEK_SET);
        if (sz != 67 * SECTOR_SIZE) {
            printf("GPT image wrong size: %d (expected %d)\n", sz, 67 * SECTOR_SIZE);
            fclose(file);
            return false;
        }
        if (fread(master_gpt, 1, 34 * SECTOR_SIZE, file) != (size_t)(34 * SECTOR_SIZE)) {
            fclose(file);
            printf("Failed to read master GPT from %s\n", paramFile);
            return false;
        }
        if (fread(backup_gpt, 1, 33 * SECTOR_SIZE, file) != (size_t)(33 * SECTOR_SIZE)) {
            fclose(file);
            printf("Failed to read backup GPT from %s\n", paramFile);
            return false;
        }
        fclose(file);
        update_gpt_disksize(master_gpt, backup_gpt, (u32)diskSectors);
        int fd = open(device, O_WRONLY | O_SYNC);
        if (fd < 0) {
            printf("Cannot open %s for writing (err=%d)\n", device, errno);
            return false;
        }
        printf("Writing master GPT...\n");
        if (!write_sectors(fd, 0, 34, master_gpt)) {
            close(fd);
            return false;
        }
        printf("Writing backup GPT...\n");
        if (!write_sectors(fd, diskSectors - 33, 33, backup_gpt)) {
            close(fd);
            return false;
        }
        fsync(fd);
        close(fd);
        printf("Writing GPT succeeded.\n");
        return true;
    }

    bRet = parse_parameter_file(paramFile, vecItems, vecUuid);
    if (!bRet) {
        printf("Parsing parameter failed!\n");
        return false;
    }
    printf("Parsed %zu partitions from parameter:\n", vecItems.size());
    for (size_t i = 0; i < vecItems.size(); i++) {
        printf("  %02zu  offset=0x%08x  size=0x%08x  %s\n",
               i, vecItems[i].uiItemOffset, vecItems[i].uiItemSize, vecItems[i].szItemName);
    }

    u8 master_gpt[34 * SECTOR_SIZE];
    u8 backup_gpt[33 * SECTOR_SIZE];
    create_gpt_buffer(master_gpt, vecItems, vecUuid, diskSectors);
    memcpy(backup_gpt, master_gpt + 2 * SECTOR_SIZE, 32 * SECTOR_SIZE);
    memcpy(backup_gpt + 32 * SECTOR_SIZE, master_gpt + SECTOR_SIZE, SECTOR_SIZE);
    prepare_gpt_backup(master_gpt, backup_gpt);

    int fd = open(device, O_WRONLY | O_SYNC);
    if (fd < 0) {
        printf("Cannot open %s for writing (err=%d)\n", device, errno);
        return false;
    }
    printf("Writing master GPT (sectors 0-33)...\n");
    if (!write_sectors(fd, 0, 34, master_gpt)) {
        close(fd);
        return false;
    }
    printf("Writing backup GPT (sectors %llu-%llu)...\n",
           (unsigned long long)(diskSectors - 33), (unsigned long long)(diskSectors - 1));
    if (!write_sectors(fd, diskSectors - 33, 33, backup_gpt)) {
        close(fd);
        return false;
    }
    fsync(fd);
    close(fd);
    printf("Writing GPT succeeded.\n");
    return true;
}

bool write_parameter_to_disk(const char *device, const char *paramFile)
{
    u8 *pParamBuf = NULL;
    UINT paramSize = 0;
    if (!make_param_buffer(paramFile, pParamBuf, paramSize)) {
        printf("Generating parameter buffer failed!\n");
        return false;
    }
    UINT nParamSec = BYTE2SECTOR(paramSize);
    if (nParamSec > 1024) {
        printf("Parameter is too large (%u sectors)!\n", nParamSec);
        delete[] pParamBuf;
        return false;
    }
    u8 *writeBuf = new u8[nParamSec * SECTOR_SIZE];
    memset(writeBuf, 0, nParamSec * SECTOR_SIZE);
    memcpy(writeBuf, pParamBuf, paramSize);

    int fd = open(device, O_WRONLY | O_SYNC);
    if (fd < 0) {
        printf("Cannot open %s for writing (err=%d)\n", device, errno);
        delete[] pParamBuf;
        delete[] writeBuf;
        return false;
    }
    printf("Writing parameter to sector 0x%x (%u sectors)...\n", PARAM_OFFSET_SECTOR, nParamSec);
    bool ok = write_sectors(fd, PARAM_OFFSET_SECTOR, nParamSec, writeBuf);
    fsync(fd);
    close(fd);
    delete[] pParamBuf;
    delete[] writeBuf;
    if (ok)
        printf("Writing parameter succeeded.\n");
    else
        printf("Writing parameter failed!\n");
    return ok;
}

bool write_image_to_partition(const char *device, UINT partOffset, UINT partSize, const char *imageFile)
{
    FILE *file = fopen(imageFile, "rb");
    if (!file) {
        printf("write_image: can't open %s (err=%d)\n", imageFile, errno);
        return false;
    }
    fseeko(file, 0, SEEK_END);
    long long iFileSize = ftello(file);
    fseeko(file, 0, SEEK_SET);

    if (partSize != 0xFFFFFFFF) {
        UINT partBytes = partSize * SECTOR_SIZE;
        if (iFileSize > (long long)partBytes) {
            printf("write_image: image %s (%lld bytes) larger than partition (%u bytes)!\n",
                   imageFile, iFileSize, partBytes);
            fclose(file);
            return false;
        }
    }

    int fd = open(device, O_WRONLY | O_SYNC);
    if (fd < 0) {
        printf("Cannot open %s for writing (err=%d)\n", device, errno);
        fclose(file);
        return false;
    }

    long long iTotalWrite = 0;
    UINT uiBegin = partOffset;
    int nSectorSize = SECTOR_SIZE;
    u8 pBuf[nSectorSize * 128];
    bool ok = true;

    printf("Writing %s (%lld bytes) to %s at sector 0x%x...\n",
           imageFile, iFileSize, device, partOffset);
    while (iTotalWrite < iFileSize) {
        memset(pBuf, 0, sizeof(pBuf));
        UINT iRead = fread(pBuf, 1, sizeof(pBuf), file);
        if (iRead == 0)
            break;
        UINT uiLen = ((iRead % 512) == 0) ? (iRead / 512) : (iRead / 512 + 1);
        if (!write_sectors(fd, uiBegin, uiLen, pBuf)) {
            printf("Write failed at sector %u!\n", uiBegin);
            ok = false;
            break;
        }
        uiBegin += uiLen;
        iTotalWrite += iRead;
        if (iFileSize > 0) {
            printf("\rWriting image: %lld%%", (iTotalWrite * 100) / iFileSize);
            fflush(stdout);
        }
    }
    printf("\n");
    fsync(fd);
    close(fd);
    fclose(file);
    if (ok)
        printf("Writing image succeeded.\n");
    return ok;
}

static bool run_cmd(const char *cmd)
{
    int ret = system(cmd);
    if (ret != 0)
        printf("Command failed (ret=%d): %s\n", ret, cmd);
    return (ret == 0);
}

bool format_partition(const char *device, UINT partOffset, UINT partSize, const char *partName, int partIndex)
{
    char cmd[512];
    char partDev[256];
    snprintf(partDev, sizeof(partDev), "%s", device);
    if (strncmp(device, "/dev/", 5) == 0) {
        size_t devLen = strlen(device);
        if (devLen > 3 && isdigit(device[devLen - 1]))
            snprintf(partDev, sizeof(partDev), "%sp%d", device, partIndex + 1);
        else
            snprintf(partDev, sizeof(partDev), "%s%d", device, partIndex + 1);
    }

    string name = partName;
    string::size_type colonPos = name.find_first_of(':');
    if (colonPos != string::npos)
        name = name.substr(0, colonPos);

    printf("Formatting partition '%s' at sector 0x%x...\n", name.c_str(), partOffset);

    if (partSize == 0xFFFFFFFF) {
        printf("Grow partition - skipping format (need exact size).\n");
        return true;
    }

    bool isExt4 = true;
    if ((name.find("boot") != string::npos) || (name.find("loader") != string::npos) ||
        (name.find("atf") != string::npos) || (name.find("reserved") != string::npos) ||
        (name.find("resource") != string::npos)) {
        isExt4 = false;
    }

    snprintf(cmd, sizeof(cmd), "partprobe %s 2>/dev/null; sleep 1", device);
    run_cmd(cmd);

    if (access(partDev, F_OK) == 0) {
        printf("Using partition device %s\n", partDev);
        if (isExt4)
            snprintf(cmd, sizeof(cmd), "mkfs.ext4 -F %s", partDev);
        else
            snprintf(cmd, sizeof(cmd), "mkfs.vfat -I %s", partDev);
        bool ok = run_cmd(cmd);
        if (ok)
            printf("Format succeeded.\n");
        return ok;
    }

    printf("Partition device %s not found, formatting via loop device...\n", partDev);
    UINT partBytes = partSize * SECTOR_SIZE;

    char loopDev[64] = {0};
    FILE *fp = popen("losetup -f 2>/dev/null", "r");
    if (fp) {
        if (fgets(loopDev, sizeof(loopDev), fp) != NULL) {
            size_t len = strlen(loopDev);
            while (len > 0 && (loopDev[len-1] == '\n' || loopDev[len-1] == '\r'))
                loopDev[--len] = '\0';
        }
        pclose(fp);
    }

    if (strlen(loopDev) > 0) {
        printf("Using loop device %s\n", loopDev);
        snprintf(cmd, sizeof(cmd), "losetup -o %u --sizelimit %u %s %s",
                 partOffset * SECTOR_SIZE, partBytes, loopDev, device);
        if (run_cmd(cmd)) {
            if (isExt4)
                snprintf(cmd, sizeof(cmd), "mkfs.ext4 -F %s", loopDev);
            else
                snprintf(cmd, sizeof(cmd), "mkfs.vfat -I %s", loopDev);
            bool ok = run_cmd(cmd);
            char detach[256];
            snprintf(detach, sizeof(detach), "losetup -d %s", loopDev);
            run_cmd(detach);
            if (ok)
                printf("Format succeeded.\n");
            return ok;
        }
    }

    printf("Loop device unavailable, trying mkfs with offset on raw device...\n");
    if (isExt4) {
        snprintf(cmd, sizeof(cmd), "mkfs.ext4 -F -E offset=%u %s %u",
                 partOffset * SECTOR_SIZE, device, partSize / 2);
    } else {
        snprintf(cmd, sizeof(cmd), "mkfs.vfat -I --offset=%u %s %u",
                 partOffset / 2, device, partSize / 2);
    }
    bool ok = run_cmd(cmd);
    if (ok)
        printf("Format succeeded.\n");
    return ok;
}

static bool find_image_for_partition(const char *imageDir, const char *partName, string &imagePath)
{
    string name = partName;
    string::size_type colonPos = name.find_first_of(':');
    if (colonPos != string::npos)
        name = name.substr(0, colonPos);

    string candidates[] = {
        string(imageDir) + "/" + name + ".img",
        string(imageDir) + "/" + name + ".raw",
        string(imageDir) + "/" + name,
        string(imageDir) + "/_" + name + ".img",
    };

    for (int i = 0; i < 4; i++) {
        struct stat st;
        if (stat(candidates[i].c_str(), &st) == 0 && S_ISREG(st.st_mode)) {
            imagePath = candidates[i];
            return true;
        }
    }
    return false;
}

bool write_all(const char *device, const char *paramFile, const char *imageDir, bool doFormat)
{
    u64 diskSectors = get_disk_sectors(device);
    if (diskSectors == 0) {
        printf("Failed to get disk size for %s\n", device);
        return false;
    }
    printf("Disk %s: %llu sectors (%llu MB)\n", device,
           (unsigned long long)diskSectors, (unsigned long long)(diskSectors / 2048));

    PARAM_ITEM_VECTOR vecItems;
    CONFIG_ITEM_VECTOR vecUuid;
    if (!parse_parameter_file(paramFile, vecItems, vecUuid)) {
        printf("Parsing parameter failed!\n");
        return false;
    }

    printf("\n=== Step 1: Writing GPT partition table ===\n");
    u8 master_gpt[34 * SECTOR_SIZE];
    u8 backup_gpt[33 * SECTOR_SIZE];
    create_gpt_buffer(master_gpt, vecItems, vecUuid, diskSectors);
    memcpy(backup_gpt, master_gpt + 2 * SECTOR_SIZE, 32 * SECTOR_SIZE);
    memcpy(backup_gpt + 32 * SECTOR_SIZE, master_gpt + SECTOR_SIZE, SECTOR_SIZE);
    prepare_gpt_backup(master_gpt, backup_gpt);

    int fd = open(device, O_WRONLY | O_SYNC);
    if (fd < 0) {
        printf("Cannot open %s for writing (err=%d)\n", device, errno);
        return false;
    }
    if (!write_sectors(fd, 0, 34, master_gpt)) {
        close(fd);
        return false;
    }
    if (!write_sectors(fd, diskSectors - 33, 33, backup_gpt)) {
        close(fd);
        return false;
    }
    fsync(fd);
    close(fd);
    printf("GPT written.\n");

    printf("\n=== Step 2: Writing parameter ===\n");
    write_parameter_to_disk(device, paramFile);

    if (imageDir && strlen(imageDir) > 0) {
        printf("\n=== Step 3: Writing images from %s ===\n", imageDir);
        for (size_t i = 0; i < vecItems.size(); i++) {
            string imagePath;
            if (find_image_for_partition(imageDir, vecItems[i].szItemName, imagePath)) {
                printf("\n--- Partition '%s' (offset=0x%x, size=0x%x) ---\n",
                       vecItems[i].szItemName, vecItems[i].uiItemOffset, vecItems[i].uiItemSize);
                write_image_to_partition(device, vecItems[i].uiItemOffset, vecItems[i].uiItemSize,
                                         imagePath.c_str());
            } else {
                printf("\n--- Partition '%s': no image found, skipping ---\n",
                       vecItems[i].szItemName);
            }
        }
    }

    if (doFormat) {
        printf("\n=== Step 4: Formatting partitions ===\n");
        run_cmd("partprobe 2>/dev/null; sleep 2");
        for (size_t i = 0; i < vecItems.size(); i++) {
            string name = vecItems[i].szItemName;
            if (name.find("loader") != string::npos || name.find("reserved") != string::npos ||
                name.find("atf") != string::npos) {
                printf("Skipping format for boot partition '%s'\n", name.c_str());
                continue;
            }
            string imagePath;
            if (imageDir && find_image_for_partition(imageDir, name.c_str(), imagePath)) {
                printf("Skipping format for '%s' (has image)\n", name.c_str());
                continue;
            }
            format_partition(device, vecItems[i].uiItemOffset, vecItems[i].uiItemSize, name.c_str(), (int)i);
        }
    }

    printf("\n=== Done! ===\n");
    return true;
}

static void print_gpt_from_disk(const char *device)
{
    int fd = open(device, O_RDONLY);
    if (fd < 0) {
        printf("Cannot open %s (err=%d)\n", device, errno);
        return;
    }
    u8 master_gpt[34 * SECTOR_SIZE];
    if (!read_sectors(fd, 0, 34, master_gpt)) {
        close(fd);
        return;
    }
    close(fd);

    gpt_header *gptHead = (gpt_header *)(master_gpt + SECTOR_SIZE);
    if (gptHead->signature != GPT_HEADER_SIGNATURE) {
        printf("No valid GPT found on %s\n", device);
        return;
    }

    printf("**********Partition Info(GPT) on %s**********\n", device);
    printf("NO  LBA(start)  LBA(end)    Name\n");
    u8 zerobuf[GPT_ENTRY_SIZE];
    memset(zerobuf, 0, GPT_ENTRY_SIZE);
    for (u32 i = 0; i < gptHead->num_partition_entries; i++) {
        gpt_entry *gptEntry = (gpt_entry *)(master_gpt + 2 * SECTOR_SIZE + i * GPT_ENTRY_SIZE);
        if (memcmp(zerobuf, (u8 *)gptEntry, GPT_ENTRY_SIZE) == 0)
            break;
        char partName[36];
        memset(partName, 0, 36);
        u32 j = 0;
        while (gptEntry->partition_name[j]) {
            partName[j] = (char)gptEntry->partition_name[j];
            j++;
        }
        printf("%02u  0x%08llx  0x%08llx  %s\n", i,
               (unsigned long long)gptEntry->starting_lba,
               (unsigned long long)gptEntry->ending_lba, partName);
    }
}

static bool get_lba_from_param_file(const char *paramFile, const char *partName, UINT &partOffset, UINT &partSize, int &partIndex)
{
    PARAM_ITEM_VECTOR vecItems;
    CONFIG_ITEM_VECTOR vecUuid;
    if (!parse_parameter_file(paramFile, vecItems, vecUuid))
        return false;
    for (size_t i = 0; i < vecItems.size(); i++) {
        string name = vecItems[i].szItemName;
        string::size_type colonPos = name.find_first_of(':');
        if (colonPos != string::npos)
            name = name.substr(0, colonPos);
        if (strcasecmp(name.c_str(), partName) == 0) {
            partOffset = vecItems[i].uiItemOffset;
            partSize = vecItems[i].uiItemSize;
            partIndex = (int)i;
            return true;
        }
    }
    return false;
}

void usage_local()
{
    printf("\r\n---------- rklocaltool (no libusb, local disk) ----------\r\n");
    printf("Usage: rklocaltool <command> [args]\r\n");
    printf("\r\n");
    printf("Commands:\r\n");
    printf("  gpt  <device> <parameter.txt>          Write GPT partition table from parameter\r\n");
    printf("  prm  <device> <parameter.txt>          Write parameter to disk (sector 0x2000)\r\n");
    printf("  wl   <device> <offset_sectors> <img>   Write raw image to sector offset\r\n");
    printf("  wlx  <device> <parameter> <part_name> <img>  Write image to named partition\r\n");
    printf("  fmt  <device> <parameter> <part_name>  Format a partition (mkfs)\r\n");
    printf("  ppt  <device>                          Print partition table (GPT)\r\n");
    printf("  all  <device> <parameter> [img_dir] [--format]  Do everything\r\n");
    printf("    all: writes GPT + parameter, then writes images from img_dir\r\n");
    printf("         (named <part_name>.img), optionally formats empty partitions\r\n");
    printf("\r\n");
    printf("Examples:\r\n");
    printf("  rklocaltool gpt /dev/sda parameter.txt\r\n");
    printf("  rklocaltool all /dev/sda parameter.txt images/ --format\r\n");
    printf("  rklocaltool wlx /dev/sda parameter.txt rootfs rootfs.img\r\n");
    printf("----------------------------------------------------------\r\n\r\n");
}

int main(int argc, char *argv[])
{
    if (argc < 2) {
        usage_local();
        return -1;
    }

    string strCmd = argv[1];
    transform(strCmd.begin(), strCmd.end(), strCmd.begin(), (int (*)(int))toupper);

    srand(time(NULL));

    if (strCmd == "-H" || strCmd == "--HELP") {
        usage_local();
        return 0;
    }

    if (strCmd == "GPT") {
        if (argc < 4) { printf("Usage: rklocaltool gpt <device> <parameter.txt>\n"); return -1; }
        return write_gpt_to_disk(argv[2], argv[3]) ? 0 : -1;
    }

    if (strCmd == "PRM") {
        if (argc < 4) { printf("Usage: rklocaltool prm <device> <parameter.txt>\n"); return -1; }
        return write_parameter_to_disk(argv[2], argv[3]) ? 0 : -1;
    }

    if (strCmd == "WL") {
        if (argc < 5) { printf("Usage: rklocaltool wl <device> <offset_sectors> <image_file>\n"); return -1; }
        UINT uiBegin = strtoul(argv[3], NULL, 0);
        return write_image_to_partition(argv[2], uiBegin, 0xFFFFFFFF, argv[4]) ? 0 : -1;
    }

    if (strCmd == "WLX") {
        if (argc < 6) { printf("Usage: rklocaltool wlx <device> <parameter> <part_name> <image_file>\n"); return -1; }
        UINT partOffset, partSize;
        int partIndex = 0;
        if (!get_lba_from_param_file(argv[3], argv[4], partOffset, partSize, partIndex)) {
            printf("Partition '%s' not found in %s\n", argv[4], argv[3]);
            return -1;
        }
        printf("Partition '%s': offset=0x%x, size=0x%x\n", argv[4], partOffset, partSize);
        return write_image_to_partition(argv[2], partOffset, partSize, argv[5]) ? 0 : -1;
    }

    if (strCmd == "FMT") {
        if (argc < 5) { printf("Usage: rklocaltool fmt <device> <parameter> <part_name>\n"); return -1; }
        UINT partOffset, partSize;
        int partIndex = 0;
        if (!get_lba_from_param_file(argv[3], argv[4], partOffset, partSize, partIndex)) {
            printf("Partition '%s' not found in %s\n", argv[4], argv[3]);
            return -1;
        }
        printf("Partition '%s': offset=0x%x, size=0x%x\n", argv[4], partOffset, partSize);
        return format_partition(argv[2], partOffset, partSize, argv[4], partIndex) ? 0 : -1;
    }

    if (strCmd == "PPT") {
        if (argc < 3) { printf("Usage: rklocaltool ppt <device>\n"); return -1; }
        print_gpt_from_disk(argv[2]);
        return 0;
    }

    if (strCmd == "ALL") {
        if (argc < 4) { printf("Usage: rklocaltool all <device> <parameter> [image_dir] [--format]\n"); return -1; }
        const char *imageDir = NULL;
        bool doFormat = false;
        for (int i = 4; i < argc; i++) {
            if (strcmp(argv[i], "--format") == 0 || strcmp(argv[i], "-f") == 0)
                doFormat = true;
            else
                imageDir = argv[i];
        }
        return write_all(argv[2], argv[3], imageDir, doFormat) ? 0 : -1;
    }

    printf("Unknown command: %s\n", argv[1]);
    usage_local();
    return -1;
}
